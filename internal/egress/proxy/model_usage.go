// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"cmp"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/modelusage"
)

// ruleSourceLLMUsage marks the row carrying one metered model call's token counts.
const ruleSourceLLMUsage = "llm:usage"

// usageFieldBytes and usagePathBytes cut a usage row's strings, so the row has a fixed size bound
// however long the request target, and spoolUsageRecordBytes is that bound
// (TestUsageRecordFitsItsReservation).
const (
	usageFieldBytes       = 256
	usagePathBytes        = 512
	spoolUsageRecordBytes = 12 << 10
)

// usageTicketKey holds a metered request's second spool reservation, for its usage row.
type usageTicketKey struct{}

// reserveUsageRow reserves a metered request's usage row before the request is forwarded, beside the
// reservation its allow row spends: the allow row is sent before the answer, so without its own
// reservation the usage row could find the sink's last credit spent. No room refuses the request, as
// a request the sink has no room for is refused, so a forwarded call never loses its usage row.
func (p *Proxy) reserveUsageRow(ctx context.Context, r *http.Request) (uuid.UUID, error) {
	if _, _, metered := p.usageShape(r); !metered {
		return uuid.Nil, nil
	}
	return p.sink.reserve(ctx, spoolUsageRecordBytes)
}

// releaseUsageRow gives back a usage reservation its row did not spend (an answer that was not
// metered, or a request that ended before one).
func (p *Proxy) releaseUsageRow(id uuid.UUID) {
	if id != uuid.Nil {
		p.sink.release(id)
	}
}

// maxDecodedUsageBytes bounds how much a compressed answer is inflated for the meter. Past it the
// meter stops reading (the answer still relays whole) and the call is uncounted, body_oversize.
const maxDecodedUsageBytes = 256 << 20

// usageShape is the metered format of r: a POST to a path naming a model API the parsers read, on a
// proxy that meters and a sink whose reader replays usage rows.
func (p *Proxy) usageShape(r *http.Request) (modelusage.Shape, string, bool) {
	if p.meterOff || !p.sink.takesUsage() || r.Method != http.MethodPost || r.URL == nil {
		return "", "", false
	}
	return modelusage.Classify(r.URL.EscapedPath())
}

// takesUsage reports whether the sink's reader replays usage rows. A runner's spool is replayed by
// wardyn-runnerd, which may predate them: its spool hash would refuse the row and stop replay. So the
// remote sink meters only while the connected runner's accept says it replays them, and a local spool
// never does: only a runner from before usage rows still has its proxy write the spool itself
// (preparedSpool). The control plane's own endpoint upgrades before proxies do.
func (s *decisionSink) takesUsage() bool {
	switch {
	case s == nil || s.local != nil:
		return false
	case s.remote != nil:
		return s.remote.usage.Load()
	}
	return true
}

// meteredSource is the rule source of a model call forwardInspectedLLM meters: the brokered route
// (hosted providers, gateways, local model servers), a terminated model host, and an Azure Foundry
// host's gated inference calls. Artifact hosts, Azure DevOps and the rest are never metered.
func meteredSource(ruleSource string) bool {
	return ruleSource == ruleSourceLLM || ruleSource == ruleSourceLLMMITM || ruleSource == ruleSourceAzure
}

// meterUsage wraps a 2xx answer to a metered call so one usage row is sent once it has been
// relayed (at EOF, or at Close for a cut stream). The bytes pass through unchanged; nothing about
// the request or the answer's headers changes. gatewayCallID is the attempt's gateway id, if any.
func (p *Proxy) meterUsage(r *http.Request, host string, port int, resp *http.Response, gatewayCallID string) {
	shape, pathModel, ok := p.usageShape(r)
	if !ok || p.sink == nil || resp.StatusCode < 200 || resp.StatusCode > 299 || resp.Body == nil {
		return
	}
	// On a runner's sink a call meters only if its admission reserved the usage row: the runner's
	// answer can change on a reconnect while the call waits, and a row with no reservation could be
	// dropped. usageShape above still turns metering off for a runner that stopped taking the rows.
	ticket, _ := r.Context().Value(usageTicketKey{}).(uuid.UUID)
	if (p.sink.local != nil || p.sink.remote != nil) && ticket == uuid.Nil {
		return
	}
	var req modelusage.Request
	if body, held := r.Context().Value(chargedBodyKey{}).([]byte); held {
		req = modelusage.ReadRequest(body) // only bytes the proxy already holds; never read again for this
	}
	parser := modelusage.NewParser(shape, modelusage.Options{
		Stream:       req.Stream || eventStream(resp.Header),
		IncludeUsage: req.IncludeUsage,
		RequestModel: cutUsageField(cmp.Or(req.Model, pathModel), usageFieldBytes),
		Header:       resp.Header,
	})
	m := &usageMeter{body: resp.Body, parser: parser, feed: parser}
	m.emit = func(u modelusage.Usage) {
		if !p.sink.takesUsage() { // the runner reconnected as one that cannot read the row
			return
		}
		dl := decisionLog(p.reqOf(r, host, port), egress.Allow, ruleSourceLLMUsage)
		dl.Request.AuditReservation = ticket
		dl.Request.Host = cutUsageField(dl.Request.Host, usageFieldBytes)
		dl.Request.Path = cutUsageField(dl.Request.Path, usagePathBytes)
		dl.GatewayCallID = gatewayCallID
		dl.Usage = &egress.ModelUsage{
			CallID: uuid.New(), Vendor: usageVendor(shape), Host: dl.Request.Host,
			Model: cutUsageField(u.Model, usageFieldBytes),
			Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning,
			State: string(u.State), Reason: string(u.Reason),
		}
		p.sink.emit(dl)
	}
	m.decodeFrom(resp.Header)
	resp.Body = m
}

// usageMeter tees an answer into its parser. A compressed answer is inflated on a goroutine fed
// through a pipe; the relay's bytes are never changed or held past the Write that copies them.
type usageMeter struct {
	body    io.ReadCloser
	parser  modelusage.Parser
	feed    io.Writer // the parser, the inflater's pipe, or nil once metering stopped
	pw      *io.PipeWriter
	done    chan struct{}
	ended   atomic.Bool // the answer reached EOF
	cutting atomic.Bool // closed before EOF: the parser reports the cut, not the inflater
	over    atomic.Bool // the inflated answer passed maxDecodedUsageBytes
	once    sync.Once
	emit    func(modelusage.Usage)
}

func (m *usageMeter) Read(b []byte) (int, error) {
	n, err := m.body.Read(b)
	if n > 0 && m.feed != nil {
		m.write(b[:n])
	}
	if err == io.EOF {
		m.ended.Store(true)
		m.finish()
	}
	return n, err
}

func (m *usageMeter) Close() error {
	err := m.body.Close()
	m.finish()
	return err
}

// write feeds one chunk; a panic or a failed inflater stops metering, never the relay.
func (m *usageMeter) write(b []byte) {
	defer func() {
		if recover() != nil {
			m.feed = nil
		}
	}()
	if _, err := m.feed.Write(b); err != nil {
		m.feed = nil
	}
}

// decodeFrom sets up the inflater for the answer's Content-Encoding. A coding it cannot inflate
// (br, zstd, a stack of codings) makes the call uncounted, undecodable.
func (m *usageMeter) decodeFrom(h http.Header) {
	coding := strings.ToLower(strings.TrimSpace(strings.Join(h.Values("Content-Encoding"), ",")))
	var open func(io.Reader) (io.Reader, error)
	switch coding {
	case "", "identity":
		return
	case "gzip", "x-gzip":
		open = func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
	case "deflate":
		open = func(r io.Reader) (io.Reader, error) { return zlib.NewReader(r) }
	default:
		m.parser.Undecodable()
		m.feed = nil
		return
	}
	pr, pw := io.Pipe()
	m.pw, m.feed, m.done = pw, pw, make(chan struct{})
	go func() {
		defer close(m.done)
		var err error
		defer func() {
			if recover() != nil {
				err = errors.New("inflater fault")
				m.parser.Undecodable()
			}
			_ = pr.CloseWithError(errors.Join(err, io.ErrClosedPipe)) // a later Write fails at once
		}()
		var dec io.Reader
		if dec, err = open(pr); err == nil {
			var n int64
			n, err = io.Copy(m.parser, io.LimitReader(dec, maxDecodedUsageBytes+1))
			if n > maxDecodedUsageBytes {
				m.over.Store(true)
				return
			}
		}
		if err != nil && !m.cutting.Load() {
			m.parser.Undecodable()
		}
	}()
}

// finish sends the one usage row: at EOF, or at the first Close.
func (m *usageMeter) finish() {
	m.once.Do(func() {
		if m.pw != nil {
			if !m.ended.Load() {
				m.cutting.Store(true)
			}
			_ = m.pw.Close()
			<-m.done
		}
		m.feed = nil
		u := m.parser.Result()
		if m.over.Load() {
			u = modelusage.Usage{Model: u.Model, State: modelusage.StateUncounted, Reason: modelusage.ReasonBodyOversize}
		}
		m.emit(u)
	})
}

// eventStream reports an SSE answer.
func eventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "text/event-stream"
}

// usageVendor is the dialect a shape's answer is in.
func usageVendor(s modelusage.Shape) string {
	switch s {
	case modelusage.ShapeAnthropicMessages:
		return "anthropic"
	case modelusage.ShapeOpenAIChat, modelusage.ShapeOpenAIResponses:
		return "openai"
	case modelusage.ShapeOllama:
		return "ollama"
	}
	return "bedrock"
}

// cutUsageField cuts s to at most n bytes on a rune boundary. A byte that is not valid UTF-8 becomes
// U+FFFD (the form the row's JSON would carry), so one stray byte never shortens the field.
func cutUsageField(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
