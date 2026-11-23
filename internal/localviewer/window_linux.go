// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package localviewer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Available says whether this computer can draw a window now: on Linux, when an X display is set.
func Available() error {
	if os.Getenv("DISPLAY") == "" {
		return ErrNoDisplay
	}
	return nil
}

// Run shows the RFB display on conn in an X11 window titled title. It returns nil when the person closes
// the window, ctx's error when ctx ends, and otherwise why the stream ended. conn is closed on return.
func Run(ctx context.Context, conn io.ReadWriteCloser, title string) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // ends a handshake ctx outlives
	defer stop()
	x, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("localviewer: cannot reach the X display: %w", err)
	}
	defer x.Close()
	pf, depth, err := screenFormat(x)
	if err != nil {
		return err
	}
	s, err := handshake(conn, pf)
	if err != nil {
		return err
	}
	w, err := newWindow(x, s.fb, depth, title)
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- s.serve(w.draw) }()
	go func() { errc <- w.events(s) }()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errc:
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return err // the deferred closes end the other goroutine
}

// screenFormat is the pixel format of the default screen's visual, as RFB asks for it: 32 bits per pixel
// in the X server's byte order, so the server's pixels go to PutImage unchanged.
func screenFormat(x *xgb.Conn) (PixelFormat, byte, error) {
	setup := xproto.Setup(x)
	screen := setup.DefaultScreen(x)
	depth := screen.RootDepth
	bpp := 0
	for _, f := range setup.PixmapFormats {
		if f.Depth == depth {
			bpp = int(f.BitsPerPixel)
		}
	}
	if bpp != 32 {
		return PixelFormat{}, 0, fmt.Errorf("localviewer: the X display draws depth %d at %d bits per pixel; the window needs 32", depth, bpp)
	}
	var vis *xproto.VisualInfo
	for _, d := range screen.AllowedDepths {
		for i := range d.Visuals {
			if d.Depth == depth && d.Visuals[i].VisualId == screen.RootVisual {
				vis = &d.Visuals[i]
			}
		}
	}
	if vis == nil || vis.Class != xproto.VisualClassTrueColor {
		return PixelFormat{}, 0, errors.New("localviewer: the X display's screen is not true colour")
	}
	pf := PixelFormat{BigEndian: setup.ImageByteOrder == xproto.ImageOrderMSBFirst}
	pf.RedShift, pf.RedMax = channel(vis.RedMask)
	pf.GreenShift, pf.GreenMax = channel(vis.GreenMask)
	pf.BlueShift, pf.BlueMax = channel(vis.BlueMask)
	return pf, depth, nil
}

// channel is a colour mask's shift and maximum.
func channel(mask uint32) (uint8, uint16) {
	if mask == 0 {
		return 0, 0
	}
	shift := bits.TrailingZeros32(mask)
	return uint8(shift), uint16(mask >> shift)
}

// window is the X11 window that shows the framebuffer.
type window struct {
	x         *xgb.Conn
	id        xproto.Window
	gc        xproto.Gcontext
	fb        *Framebuffer
	depth     byte
	maxRows   int // rows one PutImage request may carry
	protocols xproto.Atom
	deleteWin xproto.Atom
}

func newWindow(x *xgb.Conn, fb *Framebuffer, depth byte, title string) (*window, error) {
	setup := xproto.Setup(x)
	screen := setup.DefaultScreen(x)
	id, err := xproto.NewWindowId(x)
	if err != nil {
		return nil, err
	}
	mask := uint32(xproto.EventMaskExposure | xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease |
		xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease | xproto.EventMaskPointerMotion |
		xproto.EventMaskStructureNotify | xproto.EventMaskFocusChange)
	if err := xproto.CreateWindowChecked(x, depth, id, screen.Root, 0, 0, uint16(fb.W), uint16(fb.H), 0,
		xproto.WindowClassInputOutput, screen.RootVisual, xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{screen.BlackPixel, mask}).Check(); err != nil {
		return nil, fmt.Errorf("localviewer: creating the window: %w", err)
	}
	gc, err := xproto.NewGcontextId(x)
	if err != nil {
		return nil, err
	}
	if err := xproto.CreateGCChecked(x, gc, xproto.Drawable(id), 0, nil).Check(); err != nil {
		return nil, fmt.Errorf("localviewer: creating the window's graphics context: %w", err)
	}
	w := &window{x: x, id: id, gc: gc, fb: fb, depth: depth}
	// One PutImage request is at most MaximumRequestLength 4-byte units, 24 bytes of them its header.
	w.maxRows = max(1, (int(setup.MaximumRequestLength)*4-24)/(fb.W*bytesPerPixel))
	utf8, err := atom(x, "UTF8_STRING")
	if err != nil {
		return nil, err
	}
	netName, err := atom(x, "_NET_WM_NAME")
	if err != nil {
		return nil, err
	}
	if w.protocols, err = atom(x, "WM_PROTOCOLS"); err != nil {
		return nil, err
	}
	if w.deleteWin, err = atom(x, "WM_DELETE_WINDOW"); err != nil {
		return nil, err
	}
	xproto.ChangeProperty(x, xproto.PropModeReplace, id, xproto.AtomWmName, xproto.AtomString, 8, uint32(len(title)), []byte(title))
	xproto.ChangeProperty(x, xproto.PropModeReplace, id, netName, utf8, 8, uint32(len(title)), []byte(title))
	del := binary.LittleEndian.AppendUint32(nil, uint32(w.deleteWin)) // xgb speaks little-endian to the server
	xproto.ChangeProperty(x, xproto.PropModeReplace, id, w.protocols, xproto.AtomAtom, 32, 1, del)
	if err := xproto.MapWindowChecked(x, id).Check(); err != nil {
		return nil, fmt.Errorf("localviewer: showing the window: %w", err)
	}
	return w, nil
}

func atom(x *xgb.Conn, name string) (xproto.Atom, error) {
	r, err := xproto.InternAtom(x, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, fmt.Errorf("localviewer: X atom %s: %w", name, err)
	}
	return r.Atom, nil
}

// draw puts the framebuffer's rectangle r on the window, in as many PutImage requests as its size needs.
// It runs on the session's reading goroutine, the only writer of the framebuffer.
func (w *window) draw(r Rect) {
	stride, row := w.fb.W*bytesPerPixel, r.W*bytesPerPixel
	for y0 := r.Y; y0 < r.Y+r.H; y0 += w.maxRows {
		n := min(w.maxRows, r.Y+r.H-y0)
		buf := make([]byte, 0, n*row)
		for y := y0; y < y0+n; y++ {
			off := y*stride + r.X*bytesPerPixel
			buf = append(buf, w.fb.Pix[off:off+row]...)
		}
		xproto.PutImage(w.x, xproto.ImageFormatZPixmap, xproto.Drawable(w.id), w.gc, uint16(r.W), uint16(n),
			int16(r.X), int16(y0), 0, w.depth, buf)
	}
}

// events turns the window's X events into RFB input until the person closes the window (nil) or the X
// connection ends. An exposed area is asked of the server again rather than redrawn here, so only the
// session's goroutine reads the framebuffer.
func (w *window) events(s *session) error {
	in := newInput(w.x)
	for {
		ev, xerr := w.x.WaitForEvent()
		if ev == nil && xerr == nil {
			return errors.New("localviewer: the X display closed")
		}
		if xerr != nil {
			continue // an error answer to an unchecked request, such as a PutImage the window outlived
		}
		var err error
		switch e := ev.(type) {
		case xproto.ExposeEvent:
			err = s.requestUpdate(false, Rect{X: int(e.X), Y: int(e.Y), W: int(e.Width), H: int(e.Height)})
		case xproto.KeyPressEvent:
			err = in.key(s, true, e.Detail, e.State)
		case xproto.KeyReleaseEvent:
			err = in.key(s, false, e.Detail, e.State)
		case xproto.ButtonPressEvent:
			err = in.button(s, true, byte(e.Detail), int(e.EventX), int(e.EventY))
		case xproto.ButtonReleaseEvent:
			err = in.button(s, false, byte(e.Detail), int(e.EventX), int(e.EventY))
		case xproto.MotionNotifyEvent:
			err = s.pointerEvent(in.mask, int(e.EventX), int(e.EventY))
		case xproto.FocusOutEvent:
			err = in.releaseAll(s)
		case xproto.ClientMessageEvent:
			if e.Type == w.protocols && e.Format == 32 && len(e.Data.Data32) > 0 && xproto.Atom(e.Data.Data32[0]) == w.deleteWin {
				return nil
			}
		case xproto.DestroyNotifyEvent:
			return nil
		}
		if err != nil {
			return err
		}
	}
}
