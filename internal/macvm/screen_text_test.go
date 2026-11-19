// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// Setup by hand's window keeps its text byte for byte: screen_darwin.m (cgo, built only on a Mac) holds
// it and shows it when the Go side passes no text, which only the edit does.
func TestSetupWindowTextUnchanged(t *testing.T) {
	b, err := os.ReadFile("screen_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	m := string(b)
	for _, want := range []string{
		`static NSString *const wardynScreenSteps =
	@"1. Create the account wardyn-admin. Its password is the one-time code.\n"
	@"2. In System Settings > General > Sharing, turn on Screen Sharing. Under its (i) button, turn on "
	@"\"VNC viewers may control screen with password\" (any password; Wardyn replaces it).\n"
	@"3. In the same pane, turn on Remote Login.";`,
		`NSString *t = title == NULL ? @"Wardyn: set up this macOS VM" : `,
		`NSString *s = steps == NULL ? wardynScreenSteps : `,
		// The strip keeps the height of all its steps (#3869).
		"a.automaticallyAdjustsSize = NO;\n\ta.view = box;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("screen_darwin.m lost setup's window text:\n%s", want)
		}
	}
	if n := strings.Count(m, "wardynScreenSteps"); n != 2 {
		t.Errorf("wardynScreenSteps is used %d times, want its definition and the NULL default", n)
	}
}

// The window's Show code control (screen_darwin.m, built only on a Mac) is pinned by its text: the words, a
// click that reads the file and nothing before it, a readout nobody can copy.
func TestSetupWindowShowCodeControl(t *testing.T) {
	b, err := os.ReadFile("screen_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	m := string(b)
	for _, want := range []string{
		`wardynScreenShowCode = @"Show code";`,
		`wardynScreenHideCode = @"Hide code";`,
		`wardynScreenCodeUnreadable = @"Can't read the code file. In Terminal, stop the command and run it again.";`,
		`self.readout.selectable = NO;`,
		// A second click hides: the code leaves the label.
		"- (void)hide {\n\tself.shown = NO;\n\tself.readout.stringValue = @\"\";",
		`[wardynScreenStrip hide]; // no code is left in the label`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("screen_darwin.m lost the Show code control's text:\n%s", want)
		}
	}
	// No copy control and nothing selectable.
	for _, banned := range []string{"NSPasteboard", "generalPasteboard", "copy:", "NSLog", "os_log", "getenv", "NSProcessInfo"} {
		if strings.Contains(m, banned) {
			t.Errorf("screen_darwin.m mentions %q: the code must not be copyable, logged or read from the environment", banned)
		}
	}
	if strings.Contains(m, "selectable = YES") {
		t.Error("a readout is selectable")
	}
	// The file is read in one place, from the click, and again on every click: not at open and not cached.
	if n := strings.Count(m, "dataWithContentsOfFile"); n != 1 {
		t.Errorf("the code file is read in %d places, want 1", n)
	}
	if n := strings.Count(m, "wardynScreenReadCode("); n != 3 {
		t.Errorf("wardynScreenReadCode appears %d times, want its definition and the Show code and Type code clicks' calls", n)
	}
	for _, click := range []string{"toggle", "typeCode"} {
		_, after, ok := strings.Cut(m, "- (void)"+click+":(id)sender {\n")
		if !ok {
			t.Fatalf("screen_darwin.m has no %s: click action", click)
		}
		body, _, _ := strings.Cut(after, "\n}\n")
		if !strings.Contains(body, "wardynScreenReadCode(self.codeFile)") {
			t.Errorf("the %s click action does not read the file:\n%s", click, body)
		}
	}
	// What is passed in is the path, never a code.
	if !strings.Contains(m, "const char *steps, const char *codeFile) {") {
		t.Error("wardynScreenShow does not take the code file's path")
	}
	// The code's JSON key is the one setupcode.go writes.
	rec, err := json.Marshal(macvm.SetupCodeRecord{Account: "a", Code: "c"})
	if err != nil || !strings.Contains(string(rec), `"code":"c"`) || !strings.Contains(m, `[@"code"]`) {
		t.Errorf("record %s (%v) and the helper's key disagree", rec, err)
	}
}

// Only setup by hand's window is made by wardynScreenShow (the edit window is a run window with the edit's
// controls, wardynScreenShowWindow), and it is called from one place; a window of another kind (a run's
// desktop view) must not be given the control, so any new caller shows up here.
func TestScreenShowHasOneCaller(t *testing.T) {
	files, err := filepath.Glob("*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || (!strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, ".m")) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "wardynScreenShow("); n > 0 && f != "screen_darwin.go" && f != "screen_darwin.m" {
			t.Errorf("%s calls wardynScreenShow: give its window no Show code control, or update this test", f)
		}
	}
}

// textFirstBoot is the fake first boot, keeping the window text it is given.
type textFirstBoot struct {
	*macvmtest.FirstBoot
	text *macvm.ScreenText
}

func (b textFirstBoot) WithScreen(text macvm.ScreenText) macvm.FirstBooter {
	*b.text = text
	return b.FirstBoot
}

// Prepare's setup by hand hands the window the code file's path, as the edit does its own window.
func TestSetupByHandWindowGetsTheCodeFilePath(t *testing.T) {
	f, _, _ := handPrep(t)
	var text macvm.ScreenText
	f.opts.FirstBoot = textFirstBoot{f.fb, &text}
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if want := (macvm.ScreenText{CodeFile: f.opts.Layout.SetupCode()}); text != want {
		t.Errorf("setup by hand's window text %+v, want %+v", text, want)
	}
}

// The edit window is the run window's code with the edit's controls (screen_darwin.*, built only on a Mac,
// pinned by its text): its close button calls back into Go off the main thread and leaves the window up
// until the guest has shut down, a screen lock does not tear it down, and its strip gets the code file. A
// run window still gets no code file and no callback.
func TestEditWindowControlsInTheRunWindow(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m, g := read("screen_darwin.m"), read("screen_darwin.go")
	for _, want := range []string{
		"void wardynScreenShowWindow(void *vm, const char *title, const char *steps, const char *codeFile, int askStop, int canTrust) {",
		"- (BOOL)windowShouldClose:(NSWindow *)sender {\n\tdispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{\n\t\twardynWindowAction(WARDYN_WINDOW_ASK_STOP);\n\t});\n\treturn NO;\n}",
		"#define WARDYN_WINDOW_ASK_STOP 0",
		"if (wardynScreenViewOnly && !wardynScreenAsksStop && wardynScreenWindow != nil) {",
		"[w addTitlebarAccessoryViewController:wardynScreenAccessory(size.width, s, cf, edit, trust)];",
		"wardynScreenAsksStop = NO;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("screen_darwin.m lost the edit window's control:\n%s", want)
		}
	}
	for _, want := range []string{
		"//export wardynWindowAction\nfunc wardynWindowAction(n C.int) {",
		"windowActionAskStop = iota",
		"C.wardynScreenShowWindow(vm, title, steps, nil, 0, 0)",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("screen_darwin.go lost the window's callback wiring:\n%s", want)
		}
	}
	if strings.Count(g, "C.wardynScreenShowWindow(") != 2 {
		t.Error("wardynScreenShowWindow is called from other than the run window and the edit window")
	}
}
