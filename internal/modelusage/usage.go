// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package modelusage reads token counts out of model-provider answers. It is pure: no network, no proxy
// wiring, and it never changes the bytes it is shown. A caller tees the response body into a Parser and asks
// for the Usage at EOF or Close.
//
// The contract is "exact or uncounted": every number comes from a provider's own usage fields, and any
// answer that cannot be read exactly (wrong shape, garbage, too big, undecodable) is reported as uncounted
// with a Reason rather than estimated. Counts are as the provider reports them: Anthropic and Bedrock
// report cache tokens separately from Input, while OpenAI's Input already includes its cached tokens.
package modelusage

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// State is the outcome of one metered call.
type State string

const (
	// StateCounted: the provider's final usage was read.
	StateCounted State = "counted"
	// StatePartial: the stream ended early; the totals are what had arrived (a lower bound).
	StatePartial State = "partial"
	// StateUncounted: nothing trustworthy was read; Reason says why.
	StateUncounted State = "uncounted"
)

// Reason explains a partial or uncounted outcome.
type Reason string

const (
	ReasonStreamWithoutUsage Reason = "stream_without_usage" // a chat stream that carried no usage chunk
	ReasonStreamCut          Reason = "stream_cut"           // the stream ended before its final usage
	ReasonBodyOversize       Reason = "body_oversize"        // body, SSE line or event frame over its cap
	ReasonParseError         Reason = "parse_error"          // malformed JSON, frame or a negative count
	ReasonNoUsage            Reason = "no_usage"             // a well-formed answer with no usage object, or without both the input and output counts
	ReasonMeterError         Reason = "meter_error"          // the parser itself panicked
	ReasonUndecodable        Reason = "undecodable"          // the caller could not decompress the body
)

// Hard memory bounds. A non-stream body is buffered whole; streams only ever hold one line or one frame.
const (
	MaxBodyBytes = 8 << 20
	MaxLineBytes = 1 << 20
	maxFrame     = 1 << 20
)

// Usage is one call's token counts.
type Usage struct {
	Model      string `json:"model,omitempty"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Reasoning  int64  `json:"reasoning"`
	State      State  `json:"state"`
	Reason     Reason `json:"reason,omitempty"`
}

// Shape is a metered response format. Stream-ness of the Anthropic, OpenAI and Ollama shapes comes from
// Options; the Bedrock stream shapes are always AWS event streams.
type Shape string

const (
	ShapeAnthropicMessages     Shape = "anthropic_messages"
	ShapeOpenAIChat            Shape = "openai_chat"
	ShapeOpenAIResponses       Shape = "openai_responses"
	ShapeBedrockInvoke         Shape = "bedrock_invoke"
	ShapeBedrockInvokeStream   Shape = "bedrock_invoke_stream"
	ShapeBedrockConverse       Shape = "bedrock_converse"
	ShapeBedrockConverseStream Shape = "bedrock_converse_stream"
	ShapeOllama                Shape = "ollama"
)

// Options carries what the parser needs from outside the response body. The zero value is valid.
type Options struct {
	// Stream: the response is SSE (Anthropic, OpenAI). Ignored by Bedrock and Ollama shapes.
	Stream bool
	// IncludeUsage: the request set stream_options.include_usage (OpenAI Chat Completions streams).
	IncludeUsage bool
	// RequestModel is the fallback when the answer names no model.
	RequestModel string
	// Header holds the response headers; Bedrock InvokeModel reports token counts there.
	Header http.Header
}

// Parser tees a response body. Write accepts the DECODED body (gzip/br/deflate already removed by the
// caller) in any chunking and never fails or retains p. If the body cannot be decoded, the caller calls
// Undecodable instead of writing. Result may be called at any time and repeatedly.
type Parser interface {
	Write(p []byte) (int, error)
	Undecodable()
	Result() Usage
}

// Classify maps a request path to a metered shape, plus the model id embedded in a Bedrock path. Anything
// else is not metered: count_tokens and batch endpoints are not billed per call here, and unknown paths
// carry no usage dialect.
func Classify(path string) (shape Shape, pathModel string, ok bool) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return ShapeOpenAIChat, "", true
	case strings.HasSuffix(path, "/responses"):
		return ShapeOpenAIResponses, "", true
	case strings.HasSuffix(path, "/messages"):
		return ShapeAnthropicMessages, "", true
	case strings.HasSuffix(path, "/api/chat"), strings.HasSuffix(path, "/api/generate"):
		return ShapeOllama, "", true
	}
	_, rest, found := strings.Cut(path, "/model/")
	if !found {
		return "", "", false
	}
	for suffix, s := range map[string]Shape{
		"/invoke-with-response-stream": ShapeBedrockInvokeStream,
		"/converse-stream":             ShapeBedrockConverseStream,
		"/invoke":                      ShapeBedrockInvoke,
		"/converse":                    ShapeBedrockConverse,
	} {
		if id, cut := strings.CutSuffix(rest, suffix); cut {
			if dec, err := url.PathUnescape(id); err == nil {
				id = dec
			}
			return s, id, true
		}
	}
	return "", "", false
}

// Request is the part of a request body the parsers need.
type Request struct {
	Model        string
	Stream       bool
	IncludeUsage bool
}

// ReadRequest extracts model, stream and stream_options.include_usage from a JSON request body. A body
// that is not JSON yields the zero Request.
func ReadRequest(body []byte) Request {
	var r struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(body, &r) != nil {
		return Request{}
	}
	return Request{Model: r.Model, Stream: r.Stream, IncludeUsage: r.StreamOptions.IncludeUsage}
}

// NewParser returns the parser for a shape.
func NewParser(shape Shape, o Options) Parser {
	p := &parser{shape: shape, o: o}
	switch shape {
	case ShapeAnthropicMessages:
		p.setup(o.Stream, p.anthropicBody, p.anthropicLine)
	case ShapeOpenAIChat:
		p.setup(o.Stream, p.chatBody, p.chatLine)
	case ShapeOpenAIResponses:
		p.setup(o.Stream, p.responsesBody, p.responsesLine)
	case ShapeOllama:
		p.setup(true, nil, p.ollamaLine)
	case ShapeBedrockInvoke:
		p.setup(false, p.invokeBody, nil)
	case ShapeBedrockConverse:
		p.setup(false, p.converseBody, nil)
	case ShapeBedrockInvokeStream:
		p.frames, p.onFrame = true, p.invokeFrame
	case ShapeBedrockConverseStream:
		p.frames, p.onFrame = true, p.converseFrame
	default:
		p.fail(ReasonNoUsage)
	}
	return p
}

// parser is the single Parser implementation; the shape files hang their handlers off it.
type parser struct {
	shape Shape
	o     Options

	u     Usage // running max per field
	seen  bool  // some usage arrived
	final bool  // the shape's terminal usage arrived with both required figures

	haveIn, haveOut bool // the required input and output figures have each arrived (possibly across events)
	done            bool // OpenAI [DONE] seen
	ended           bool // a terminal event arrived, with or without usage

	bad     Reason // sticky: first failure wins, except Undecodable
	flushed bool

	lines  bool
	frames bool
	buf    []byte

	onBody  func([]byte)
	onLine  func([]byte)
	onFrame func(frame)
}

func (p *parser) setup(stream bool, body, line func([]byte)) {
	if stream && line != nil {
		p.lines, p.onLine = true, line
		return
	}
	p.onBody = body
}

func (p *parser) fail(r Reason) {
	if p.bad == "" {
		p.bad = r
	}
	p.buf = nil
}

func (p *parser) Undecodable() {
	p.bad = ReasonUndecodable
	p.buf = nil
}

// Write always reports the whole input as written with a nil error: the parser sits in a tee on the
// provider response, and a short write or an error there would abort it. A panic is recovered into
// meter_error and leaves the named results as set here.
func (p *parser) Write(b []byte) (n int, err error) {
	n = len(b)
	if p.bad != "" || len(b) == 0 {
		return n, nil
	}
	defer func() {
		if recover() != nil {
			p.fail(ReasonMeterError)
		}
	}()
	switch {
	case p.frames:
		p.feedFrames(b)
	case p.lines:
		p.feedLines(b)
	default:
		if len(p.buf)+len(b) > MaxBodyBytes {
			p.fail(ReasonBodyOversize)
			return n, nil
		}
		p.buf = append(p.buf, b...)
	}
	return n, nil
}

func (p *parser) Result() (u Usage) {
	if !p.flushed && p.bad == "" {
		p.flushed = true
		func() {
			defer func() {
				if recover() != nil {
					p.fail(ReasonMeterError)
				}
			}()
			p.flush()
		}()
	}
	u = p.u
	u.Model = cmp.Or(u.Model, p.o.RequestModel)
	switch {
	case p.bad != "":
		return Usage{Model: u.Model, State: StateUncounted, Reason: p.bad}
	case p.final:
		u.State = StateCounted
	case p.seen:
		u.State, u.Reason = StatePartial, ReasonStreamCut
	default:
		u.State, u.Reason = StateUncounted, p.noUsageReason()
	}
	return u
}

func (p *parser) noUsageReason() Reason {
	switch {
	case p.shape == ShapeOpenAIChat && p.lines && (!p.o.IncludeUsage || p.done):
		return ReasonStreamWithoutUsage
	case p.ended:
		return ReasonNoUsage
	case p.lines || p.frames:
		return ReasonStreamCut
	}
	return ReasonNoUsage
}

// flush handles end of input: a trailing unterminated line, or the whole buffered body.
func (p *parser) flush() {
	switch {
	case p.lines:
		if len(p.buf) > 0 {
			p.line(p.buf)
			p.buf = nil
		}
	case p.onBody != nil:
		p.onBody(p.buf)
		p.buf = nil
	}
}

func (p *parser) feedLines(b []byte) {
	for len(b) > 0 && p.bad == "" {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if len(p.buf)+len(b) > MaxLineBytes {
				p.fail(ReasonBodyOversize)
				return
			}
			p.buf = append(p.buf, b...)
			return
		}
		if len(p.buf)+i > MaxLineBytes {
			p.fail(ReasonBodyOversize)
			return
		}
		if len(p.buf) == 0 {
			p.line(b[:i])
		} else {
			p.buf = append(p.buf, b[:i]...)
			p.line(p.buf)
			p.buf = p.buf[:0]
		}
		b = b[i+1:]
	}
}

func (p *parser) line(l []byte) { p.onLine(bytes.TrimRight(l, "\r")) }

// sseData returns the JSON payload of an SSE data line; event:, id:, comments and blanks are not data.
func sseData(l []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return nil, false
	}
	return bytes.TrimSpace(rest), true
}

// decode unmarshals JSON or fails the parser. Reports whether v is usable.
func (p *parser) decode(b []byte, v any) bool {
	if json.Unmarshal(b, v) != nil {
		p.fail(ReasonParseError)
		return false
	}
	return true
}

// merge folds one reading into the running totals, keeping the max per field: cumulative stream counters
// only grow, and the same figure arriving twice must not double. A negative count is a parse error.
func (p *parser) merge(in, out, cacheRead, cacheWrite, reasoning int64) {
	for _, v := range []int64{in, out, cacheRead, cacheWrite, reasoning} {
		if v < 0 {
			p.fail(ReasonParseError)
			return
		}
	}
	p.u.Input = max(p.u.Input, in)
	p.u.Output = max(p.u.Output, out)
	p.u.CacheRead = max(p.u.CacheRead, cacheRead)
	p.u.CacheWrite = max(p.u.CacheWrite, cacheWrite)
	p.u.Reasoning = max(p.u.Reasoning, reasoning)
	p.seen = true
}

// figures folds one reading into the totals like merge, where in and out are the required figures (nil
// when absent or null: never read as zero) and the rest are optional. It remembers which required figures
// have arrived, since a stream can deliver them in different events.
func (p *parser) figures(in, out *int64, cacheRead, cacheWrite, reasoning int64) {
	var i, o int64
	if in != nil {
		i, p.haveIn = *in, true
	}
	if out != nil {
		o, p.haveOut = *out, true
	}
	p.merge(i, o, cacheRead, cacheWrite, reasoning)
}

// terminal marks the shape's terminal usage. The call is counted only when both required figures have
// arrived; otherwise it is uncounted/no_usage, the provider having reported no usable figure.
func (p *parser) terminal() {
	if p.haveIn && p.haveOut {
		p.final = true
		return
	}
	p.fail(ReasonNoUsage)
}

func (p *parser) model(m string) { p.u.Model = cmp.Or(p.u.Model, m) }
