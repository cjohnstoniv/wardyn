// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

// The first boot's screen in a window on the Mac, for the base's setup by
// hand (see screen_darwin.go). AppKit runs only on the main thread:
// wardynScreenRun is called there, and every window change is a block on the
// main queue, which [NSApp run] serves. The VM stays on its own queue, as
// Code-Hex/vz's own window does.
//
// The app is an accessory (no Dock icon, no menu bar), so no Quit can end
// the prepare, and it has no delegate, so no window closing ends it either.
// Setup's window has no close button: it closes when setup by hand ends.
// The edit's window has one, which asks the guest to shut down; it closes
// once the guest has.
//
// The strip can carry a Show code control for the one-time code. The window is given only the code file's
// path; a click reads the file then (wardynScreenReadCode), shows the code in a readout that cannot be selected,
// and a second click clears it. The code is never in argv, the environment, an event or a log, and there is no
// copy control: a clipboard can leave the Mac. The edit's strip also has a Type code button, which reads the
// file the same way and presses the code's keys in the VM's view (it does not press Return), and three buttons
// that call Go (wardynWindowAction), plus a line Go rewrites (wardynScreenSetLive).

#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h> // kVK_ANSI_* key codes (HIToolbox/Events.h); no Carbon call is made
#import <CoreGraphics/CoreGraphics.h>
#import <Virtualization/Virtualization.h>
#include <stdatomic.h>

static NSWindow *wardynScreenWindow; // main thread only
// 1 once the window and its view are up, -1 if they could not be made, 0 before (and after a close).
static _Atomic int wardynScreenState;

// wardynScreenSession is 1 when a window-server (GUI) session exists, else 0 (nobody at the desktop), and
// AppKit must not start. Over SSH while someone is signed in it likely answers 1: runnerd refuses SSH.
int wardynScreenSession(void) {
	CFDictionaryRef d = CGSessionCopyCurrentDictionary();
	if (d == NULL) {
		return 0;
	}
	CFRelease(d);
	return 1;
}

// wardynScreenBoolKey reads one boolean key of d as the Go side's consoleKey: 0 absent, 1 false, 2 true, 3 not a
// boolean.
static int wardynScreenBoolKey(CFDictionaryRef d, CFStringRef key) {
	CFTypeRef v = CFDictionaryGetValue(d, key);
	if (v == NULL) {
		return 0;
	}
	if (CFGetTypeID(v) != CFBooleanGetTypeID()) {
		return 3;
	}
	return CFBooleanGetValue((CFBooleanRef)v) ? 2 : 1;
}

// wardynScreenConsoleKeys reads the run window's console check: the session's on-console key and its lock key
// (CGSSessionScreenIsLocked, undocumented), each as wardynScreenBoolKey gives it. It returns 0, setting neither,
// when the session dictionary cannot be had. It decides nothing: consoleCheck (screen.go) does. A missing lock key
// is unlocked, since macOS adds it only while the screen is locked; a malformed one, or no dictionary, is an
// unknown lock that refuses.
int wardynScreenConsoleKeys(int *onConsole, int *locked) {
	CFDictionaryRef d = CGSessionCopyCurrentDictionary();
	if (d == NULL) {
		return 0;
	}
	*onConsole = wardynScreenBoolKey(d, kCGSessionOnConsoleKey);
	*locked = wardynScreenBoolKey(d, CFSTR("CGSSessionScreenIsLocked"));
	CFRelease(d);
	return 1;
}

static void wardynScreenWatchLock(void); // below, beside the run window it closes

// wardynScreenRun starts AppKit on the main thread. It does not return. The screen's lock notifications are
// observed from here: they are delivered by this thread's run loop, which [NSApp run] serves.
void wardynScreenRun(void) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		wardynScreenWatchLock();
	}
	[NSApp run];
}

// The window's content size: the guest's 16:9 display, at most 1280 wide,
// within 90% of the screen.
static NSSize wardynScreenSize(void) {
	NSSize s = NSMakeSize(1280, 720);
	NSScreen *screen = NSScreen.mainScreen;
	if (screen != nil) {
		NSRect v = screen.visibleFrame;
		s.width = MIN(s.width, v.size.width * 0.9);
		s.height = s.width * 9 / 16;
		if (s.height > v.size.height * 0.9) {
			s.height = v.size.height * 0.9;
			s.width = s.height * 16 / 9;
		}
	}
	return s;
}

// The steps the person at the guest's screen follows. Before Remote Login there is no SSH and no guest agent,
// so the host cannot drive the guest's settings: the window says what to do.
static NSString *const wardynScreenSteps =
	@"1. Create the account wardyn-admin. Its password is the one-time code.\n"
	@"2. In System Settings > General > Sharing, turn on Screen Sharing. Under its (i) button, turn on "
	@"\"VNC viewers may control screen with password\" (any password; Wardyn replaces it).\n"
	@"3. In the same pane, turn on Remote Login.";

// The Show code control's texts.
static NSString *const wardynScreenShowCode = @"Show code";
static NSString *const wardynScreenHideCode = @"Hide code";
static NSString *const wardynScreenCodeUnreadable = @"Can't read the code file. In Terminal, stop the command and run it again.";
static NSString *const wardynScreenCodeUntypable = @"Type code can't type this code. Click Show code and type it.";

// The edit strip's buttons, in the order a person uses them, and Trust's words when it has nothing to trust.
static NSString *const wardynScreenAdminTerminal = @"Open admin Terminal";
static NSString *const wardynScreenTypeCode = @"Type code";
static NSString *const wardynScreenTrustCAs = @"Trust the list's CAs";
static NSString *const wardynScreenFullDiskAccess = @"Full Disk Access";
static NSString *const wardynScreenInstallRosetta = @"Install Rosetta";
static NSString *const wardynScreenNoCerts = @"The install list has no certificates.";

void wardynWindowAction(int n); // screen_darwin.go (cgo export): the open edit window's Go callback n

// The numbers the edit window passes to wardynWindowAction (screen_darwin.go's windowAction constants).
#define WARDYN_WINDOW_ASK_STOP 0
#define WARDYN_WINDOW_ADMIN_TERMINAL 1
#define WARDYN_WINDOW_TRUST_CAS 2
#define WARDYN_WINDOW_FULL_DISK_ACCESS 3
#define WARDYN_WINDOW_INSTALL_ROSETTA 4

// wardynScreenKeyCode is the key that types c on a US keyboard (kVK_ANSI_*), for the edit code's letters
// (typableAlphabet in controlcheck.go: on the same key on QWERTY, QWERTZ and AZERTY), or -1 for any other.
static int wardynScreenKeyCode(unichar c) {
	switch (c) {
	case 'b': return kVK_ANSI_B;
	case 'c': return kVK_ANSI_C;
	case 'd': return kVK_ANSI_D;
	case 'e': return kVK_ANSI_E;
	case 'f': return kVK_ANSI_F;
	case 'g': return kVK_ANSI_G;
	case 'h': return kVK_ANSI_H;
	case 'i': return kVK_ANSI_I;
	case 'j': return kVK_ANSI_J;
	case 'k': return kVK_ANSI_K;
	case 'l': return kVK_ANSI_L;
	case 'n': return kVK_ANSI_N;
	case 'o': return kVK_ANSI_O;
	case 'p': return kVK_ANSI_P;
	case 'r': return kVK_ANSI_R;
	case 's': return kVK_ANSI_S;
	case 't': return kVK_ANSI_T;
	case 'u': return kVK_ANSI_U;
	case 'v': return kVK_ANSI_V;
	case 'x': return kVK_ANSI_X;
	}
	return -1;
}

// wardynScreenReadCode reads the one-time code from the setup-code file at path ({"account":..., "code":...}, as
// setupcode.go writes it), or returns nil when the file is gone, unreadable or not that record. Only a click calls
// it, on every click: nothing is read before one and nothing is kept between clicks.
static NSString *wardynScreenReadCode(NSString *path) {
	NSData *data = [NSData dataWithContentsOfFile:path];
	if (data == nil) {
		return nil;
	}
	id record = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
	if (![record isKindOfClass:[NSDictionary class]]) {
		return nil;
	}
	id code = ((NSDictionary *)record)[@"code"];
	if (![code isKindOfClass:[NSString class]] || [(NSString *)code length] == 0) {
		return nil;
	}
	return code;
}

// WardynScreenStrip is the strip of steps under the title bar. With a code file, line 1 gives up the right of
// its row to a Show code button and, while the code (or why it cannot be read) shows, a readout beside it; the
// other lines keep the strip's full width. The strip is as tall as its text, so every change of the readout
// fits it again. Main thread only.
@interface WardynScreenStrip : NSObject
@property(strong) NSView *box;
@property(strong) NSTextField *first;   // line 1 (with a code file) or all the steps (without)
@property(strong) NSTextField *rest;    // lines 2 on (with a code file)
@property(strong) NSTextField *readout; // the code, or the unreadable message; empty while hidden
@property(strong) NSButton *button;     // nil without a code file
@property(strong) NSTextField *live;    // the edit window's line Go rewrites; nil for other windows
@property(strong) NSArray<NSButton *> *buttons; // the edit window's button row; empty for other windows
@property(copy) NSString *codeFile;     // the file's path, not the code
@property BOOL shown;
@property BOOL typing; // Type code's keys are being pressed; a click then does nothing
- (instancetype)initWithBox:(NSView *)box steps:(NSString *)steps codeFile:(NSString *)codeFile buttons:(BOOL)buttons canTrust:(BOOL)canTrust;
- (void)toggle:(id)sender;
- (void)typeCode:(id)sender;
- (void)act:(id)sender;
- (void)hide;
- (void)fit;
@end

@implementation WardynScreenStrip
- (instancetype)initWithBox:(NSView *)box steps:(NSString *)steps codeFile:(NSString *)codeFile buttons:(BOOL)buttons canTrust:(BOOL)canTrust {
	self = [super init];
	if (self == nil) {
		return nil;
	}
	self.box = box;
	self.codeFile = codeFile;
	NSString *line1 = steps;
	NSString *others = @"";
	NSRange nl = [steps rangeOfString:@"\n"];
	if (codeFile != nil && nl.location != NSNotFound) {
		line1 = [steps substringToIndex:nl.location];
		others = [steps substringFromIndex:nl.location + 1];
	}
	self.first = [NSTextField wrappingLabelWithString:line1];
	self.first.font = [NSFont systemFontOfSize:12];
	self.first.autoresizingMask = NSViewWidthSizable;
	[box addSubview:self.first];
	if (codeFile != nil) {
		self.rest = [NSTextField wrappingLabelWithString:others];
		self.rest.font = [NSFont systemFontOfSize:12];
		self.rest.autoresizingMask = NSViewWidthSizable;
		[box addSubview:self.rest];
		// The readout is not selectable: there is no way to copy the code out of the window.
		self.readout = [NSTextField wrappingLabelWithString:@""];
		self.readout.selectable = NO;
		self.readout.autoresizingMask = NSViewMinXMargin;
		[box addSubview:self.readout];
		self.button = [NSButton buttonWithTitle:wardynScreenShowCode target:self action:@selector(toggle:)];
		self.button.controlSize = NSControlSizeSmall;
		self.button.font = [NSFont systemFontOfSize:[NSFont systemFontSizeForControlSize:NSControlSizeSmall]];
		self.button.autoresizingMask = NSViewMinXMargin;
		[box addSubview:self.button];
	}
	self.buttons = @[];
	if (buttons) {
		self.live = [NSTextField wrappingLabelWithString:@""];
		self.live.font = [NSFont systemFontOfSize:12];
		self.live.textColor = NSColor.secondaryLabelColor;
		self.live.autoresizingMask = NSViewWidthSizable;
		[box addSubview:self.live];
		NSMutableArray<NSButton *> *row = [NSMutableArray array];
		NSButton *(^add)(NSString *, SEL, NSInteger) = ^NSButton *(NSString *title, SEL action, NSInteger tag) {
			NSButton *b = [NSButton buttonWithTitle:title target:self action:action];
			b.controlSize = NSControlSizeSmall;
			b.font = [NSFont systemFontOfSize:[NSFont systemFontSizeForControlSize:NSControlSizeSmall]];
			b.tag = tag;
			[box addSubview:b];
			[row addObject:b];
			return b;
		};
		add(wardynScreenAdminTerminal, @selector(act:), WARDYN_WINDOW_ADMIN_TERMINAL);
		if (codeFile != nil) {
			add(wardynScreenTypeCode, @selector(typeCode:), 0);
		}
		NSButton *trust = add(wardynScreenTrustCAs, @selector(act:), WARDYN_WINDOW_TRUST_CAS);
		if (!canTrust) {
			trust.enabled = NO;
			trust.toolTip = wardynScreenNoCerts;
		}
		add(wardynScreenFullDiskAccess, @selector(act:), WARDYN_WINDOW_FULL_DISK_ACCESS);
		add(wardynScreenInstallRosetta, @selector(act:), WARDYN_WINDOW_INSTALL_ROSETTA);
		self.buttons = row;
	}
	[self hide];
	return self;
}

// act is a click on a button that calls Go: off the main thread, since the callback waits on the guest.
- (void)act:(id)sender {
	int n = (int)((NSButton *)sender).tag;
	dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
		wardynWindowAction(n);
	});
}

// say shows msg in the readout, in red, instead of the code.
- (void)say:(NSString *)msg {
	self.readout.font = [NSFont systemFontOfSize:12];
	self.readout.textColor = NSColor.systemRedColor;
	self.readout.stringValue = msg;
	self.button.title = wardynScreenShowCode; // a click tries again
	self.shown = NO;
	[self fit];
}

// typeCode is Type code's click: it reads the code file, as Show code does, and presses each letter's key in
// the VM's view, 50 ms apart, then leaves the view focused for the person to press Return. The key, not the
// letter, reaches the guest, which reads it with its own keyboard layout: the code's letters are on the same
// key on QWERTY, QWERTZ and AZERTY. A code with any other character is not typed at all. While the keys are
// being pressed the button is off and a click does nothing, so two codes never interleave.
- (void)typeCode:(id)sender {
	if (self.typing) {
		return;
	}
	NSString *code = wardynScreenReadCode(self.codeFile);
	NSWindow *w = self.box.window;
	if (code == nil || ![w.contentView isKindOfClass:[VZVirtualMachineView class]]) {
		[self say:wardynScreenCodeUnreadable];
		return;
	}
	for (NSUInteger i = 0; i < code.length; i++) {
		if (wardynScreenKeyCode([code characterAtIndex:i]) < 0) {
			[self say:wardynScreenCodeUntypable];
			return;
		}
	}
	VZVirtualMachineView *view = (VZVirtualMachineView *)w.contentView;
	NSTimeInterval t0 = NSApp.currentEvent.timestamp; // the click's
	NSButton *button = sender;
	self.typing = YES;
	button.enabled = NO;
	[w makeFirstResponder:view];
	for (NSUInteger i = 0; i < code.length; i++) {
		unichar c = [code characterAtIndex:i];
		unsigned short key = (unsigned short)wardynScreenKeyCode(c);
		NSString *ch = [NSString stringWithCharacters:&c length:1];
		dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(i * 50 * NSEC_PER_MSEC)), dispatch_get_main_queue(), ^{
			if (wardynScreenWindow != w) {
				return; // the window closed while typing
			}
			for (int down = 1; down >= 0; down--) {
				NSEvent *e = [NSEvent keyEventWithType:(down ? NSEventTypeKeyDown : NSEventTypeKeyUp)
				                              location:NSZeroPoint
				                         modifierFlags:0
				                             timestamp:t0 + i * 0.05
				                          windowNumber:w.windowNumber
				                               context:nil
				                            characters:ch
				           charactersIgnoringModifiers:ch
				                             isARepeat:NO
				                               keyCode:key];
				if (down) {
					[view keyDown:e];
				} else {
					[view keyUp:e];
				}
			}
		});
	}
	// After the last key, on the same queue: the button is on again.
	__weak WardynScreenStrip *strip = self;
	dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(code.length * 50 * NSEC_PER_MSEC)), dispatch_get_main_queue(), ^{
		strip.typing = NO;
		button.enabled = YES;
	});
}

// toggle is Show code's click, one of the two places the code file is read (Type code's is the other).
- (void)toggle:(id)sender {
	if (self.shown) {
		[self hide];
		return;
	}
	NSString *code = wardynScreenReadCode(self.codeFile);
	if (code == nil) {
		[self say:wardynScreenCodeUnreadable];
		return;
	}
	// Bigger than the steps, and monospaced: setup's code alphabet keeps pairs such as c/C and s/S.
	self.readout.font = [NSFont monospacedSystemFontOfSize:16 weight:NSFontWeightMedium];
	self.readout.textColor = NSColor.labelColor;
	self.readout.stringValue = code;
	self.button.title = wardynScreenHideCode;
	self.shown = YES;
	[self fit];
}

// hide clears the readout (and with it the code) and fits the strip to the steps.
- (void)hide {
	self.shown = NO;
	self.readout.stringValue = @"";
	self.button.title = wardynScreenShowCode;
	[self fit];
}

// fit lays the strip out at the box's width and sets the box's height to what the text needs. The controller
// observes a bottom accessory's view frame (NSTitlebarAccessoryViewController: "you can change the view's
// height when layoutAttribute is bottom"), so the new height takes effect. The origin is left alone.
- (void)fit {
	const CGFloat pad = 10, gap = 8, readoutMax = 190;
	CGFloat w = self.box.bounds.size.width;
	CGFloat avail = w - 2 * pad;
	CGFloat control = 0; // what the button and readout take of line 1's row
	NSSize bs = NSZeroSize, rs = NSZeroSize;
	if (self.button != nil) {
		[self.button sizeToFit];
		bs = self.button.frame.size;
		control = gap + bs.width;
		if (self.readout.stringValue.length > 0) {
			rs.width = MIN(ceil([self.readout.attributedStringValue size].width) + 8, readoutMax);
			rs.height = ceil([self.readout.cell cellSizeForBounds:NSMakeRect(0, 0, rs.width, CGFLOAT_MAX)].height);
			control += gap + rs.width;
		}
	}
	CGFloat firstW = MAX(avail - control, 40);
	CGFloat firstH = ceil([self.first.cell cellSizeForBounds:NSMakeRect(0, 0, firstW, CGFLOAT_MAX)].height);
	CGFloat restH = 0;
	if (self.rest != nil && self.rest.stringValue.length > 0) {
		restH = ceil([self.rest.cell cellSizeForBounds:NSMakeRect(0, 0, avail, CGFLOAT_MAX)].height);
	}
	CGFloat rowH = MAX(firstH, MAX(rs.height, bs.height));
	// The edit's buttons, left to right, onto more rows when the strip is narrow; frames' y holds the row
	// until the rows are counted.
	NSMutableArray<NSValue *> *frames = [NSMutableArray array];
	CGFloat bx = pad, lineH = 0;
	NSInteger rows = 0;
	for (NSButton *b in self.buttons) {
		[b sizeToFit];
		NSSize s = b.frame.size;
		if (bx > pad && bx + s.width > w - pad) {
			rows++;
			bx = pad;
		}
		[frames addObject:[NSValue valueWithRect:NSMakeRect(bx, rows, s.width, s.height)]];
		bx += s.width + gap;
		lineH = MAX(lineH, s.height);
	}
	CGFloat buttonsH = self.buttons.count > 0 ? (rows + 1) * (lineH + 4) : 0;
	CGFloat liveH = 0;
	if (self.live != nil && self.live.stringValue.length > 0) {
		liveH = ceil([self.live.cell cellSizeForBounds:NSMakeRect(0, 0, avail, CGFLOAT_MAX)].height) + 4;
	}
	[self.box setFrameSize:NSMakeSize(w, rowH + restH + liveH + buttonsH + 8)];
	// Unflipped: y runs up from the box's bottom.
	for (NSUInteger i = 0; i < self.buttons.count; i++) {
		NSRect f = frames[i].rectValue;
		f.origin.y = 4 + (rows - f.origin.y) * (lineH + 4);
		self.buttons[i].frame = f;
	}
	self.live.frame = NSMakeRect(pad, 4 + buttonsH, avail, MAX(liveH - 4, 0));
	self.live.hidden = liveH == 0;
	self.rest.frame = NSMakeRect(pad, 4 + buttonsH + liveH, avail, restH);
	CGFloat rowY = 4 + buttonsH + liveH + restH;
	self.first.frame = NSMakeRect(pad, rowY + rowH - firstH, firstW, firstH);
	self.button.frame = NSMakeRect(w - pad - bs.width, rowY + (rowH - bs.height) / 2, bs.width, bs.height);
	self.readout.frame = NSMakeRect(w - pad - bs.width - gap - rs.width, rowY + (rowH - rs.height) / 2, rs.width, rs.height);
	self.readout.hidden = rs.width == 0;
}
@end

static WardynScreenStrip *wardynScreenStrip; // main thread only; the button's target is weak

// wardynScreenAccessory is the strip of steps under the window's title bar, with the Show code control when
// codeFile is not nil, and the edit's buttons and live line when buttons is YES.
static NSTitlebarAccessoryViewController *wardynScreenAccessory(CGFloat width, NSString *steps, NSString *codeFile, BOOL buttons, BOOL canTrust) {
	// The strip is as tall as the steps are at this width (a narrow window wraps them).
	NSView *box = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, width, 0)];
	wardynScreenStrip = [[WardynScreenStrip alloc] initWithBox:box steps:steps codeFile:codeFile buttons:buttons canTrust:canTrust];
	NSTitlebarAccessoryViewController *a = [[NSTitlebarAccessoryViewController alloc] init];
	a.layoutAttribute = NSLayoutAttributeBottom;
	// Else the controller resizes the strip to one title-bar line when given the view (macOS 26: 36 pt, one step).
	a.automaticallyAdjustsSize = NO;
	a.view = box;
	return a;
}

// wardynScreenShow opens the window on vm, a VZVirtualMachine; the block
// keeps it until the window closes. title and steps NULL are setup by
// hand's. It has no close button. codeFile, the setup-code file's path, gives the strip
// a Show code control; NULL gives it none (a window that is not setup's or the edit's must pass NULL).
void wardynScreenShow(void *vm, const char *title, const char *steps, const char *codeFile) {
	VZVirtualMachine *machine = (__bridge VZVirtualMachine *)vm;
	NSString *t = title == NULL ? @"Wardyn: set up this macOS VM" : [[NSString alloc] initWithUTF8String:title];
	NSString *s = steps == NULL ? wardynScreenSteps : [[NSString alloc] initWithUTF8String:steps];
	NSString *cf = codeFile == NULL ? nil : [[NSString alloc] initWithUTF8String:codeFile];
	dispatch_async(dispatch_get_main_queue(), ^{
		if (wardynScreenWindow == nil) {
			NSSize size = wardynScreenSize();
			NSWindowStyleMask mask = NSWindowStyleMaskTitled | NSWindowStyleMaskResizable;
			NSWindow *w = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, size.width, size.height)
			                                          styleMask:mask
			                                            backing:NSBackingStoreBuffered
			                                              defer:NO];
			w.releasedWhenClosed = NO;
			w.title = t;
			[w addTitlebarAccessoryViewController:wardynScreenAccessory(size.width, s, cf, NO, NO)];
			w.contentAspectRatio = NSMakeSize(16, 9);
			VZVirtualMachineView *view = [[VZVirtualMachineView alloc] init];
			view.virtualMachine = machine;
			w.contentView = view;
			[w center];
			wardynScreenWindow = w;
		}
		// An accessory app's window opens behind the active app: macOS 14's activate is cooperative and is
		// ignored while another app is active. So the window orders itself front regardless, follows the
		// person to their Space and floats while setup waits (it closes when setup ends).
		wardynScreenWindow.collectionBehavior |= NSWindowCollectionBehaviorMoveToActiveSpace;
		wardynScreenWindow.level = NSFloatingWindowLevel;
		[wardynScreenWindow makeKeyAndOrderFront:nil];
		[wardynScreenWindow orderFrontRegardless];
		[NSApp activate];
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
		[NSApp activateIgnoringOtherApps:YES];
#pragma clang diagnostic pop
		atomic_store(&wardynScreenState, wardynScreenWindow != nil && wardynScreenWindow.contentView != nil ? 1 : -1);
	});
}

// WardynScreenAskStop is the edit window's delegate: its close button asks the guest to shut down through
// Go, off the main thread, and the window stays up until the guest has (wardynScreenClose).
@interface WardynScreenAskStop : NSObject <NSWindowDelegate>
@end

@implementation WardynScreenAskStop
- (BOOL)windowShouldClose:(NSWindow *)sender {
	dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
		wardynWindowAction(WARDYN_WINDOW_ASK_STOP);
	});
	return NO;
}
@end

static WardynScreenAskStop *wardynScreenStopper; // main thread only; the window's delegate is weak

static BOOL wardynScreenViewOnly; // main thread only: the window is a run window (see wardynScreenShowWindow)
static BOOL wardynScreenAsksStop; // main thread only: the run window is the edit's, which a lock leaves up

// wardynScreenTeardown closes the window and lets go of its VM; main thread only. A run window first detaches its
// view from the VM, which goes on running.
static void wardynScreenTeardown(void) {
	[wardynScreenStrip hide]; // no code is left in the label
	wardynScreenStrip = nil;
	if (wardynScreenViewOnly && [wardynScreenWindow.contentView isKindOfClass:[VZVirtualMachineView class]]) {
		((VZVirtualMachineView *)wardynScreenWindow.contentView).virtualMachine = nil;
	}
	wardynScreenViewOnly = NO;
	wardynScreenAsksStop = NO;
	[wardynScreenWindow orderOut:nil];
	wardynScreenWindow.contentView = nil;
	wardynScreenWindow.delegate = nil;
	wardynScreenWindow = nil;
	wardynScreenStopper = nil;
	atomic_store(&wardynScreenState, 0);
}

// WardynScreenCloseOnly is a run window's delegate: the close button closes the window and nothing else. It never
// asks the guest to stop and never ends the process (Code-Hex/vz's own window does both).
@interface WardynScreenCloseOnly : NSObject <NSWindowDelegate>
@end

@implementation WardynScreenCloseOnly
- (BOOL)windowShouldClose:(NSWindow *)sender {
	return YES;
}
- (void)windowWillClose:(NSNotification *)notification {
	// After this call returns, so the window is not torn down inside its own delegate callback.
	dispatch_async(dispatch_get_main_queue(), ^{
		if (wardynScreenWindow == notification.object) {
			wardynScreenTeardown(); // the state goes to 0, which the Go side polls
		}
	});
}
@end

static WardynScreenCloseOnly *wardynScreenCloseOnly; // main thread only; the window's delegate is weak

// The screen's lock notifications: wardynScreenLockCount counts com.apple.screenIsLocked, and wardynScreenLockLast
// is 1 while the latest of it and com.apple.screenIsUnlocked was a lock. The Go side reads both from any thread.
static _Atomic long wardynScreenLockCount;
static _Atomic int wardynScreenLockLast;

// WardynScreenLockWatch observes the lock notifications. A lock closes a run window at once, whatever the session
// dictionary says; the count goes up first, so the Go side reads the close as the lock's.
@interface WardynScreenLockWatch : NSObject
- (void)locked:(NSNotification *)n;
- (void)unlocked:(NSNotification *)n;
@end

@implementation WardynScreenLockWatch
- (void)locked:(NSNotification *)n {
	atomic_store(&wardynScreenLockLast, 1);
	atomic_fetch_add(&wardynScreenLockCount, 1);
	dispatch_async(dispatch_get_main_queue(), ^{
		if (wardynScreenViewOnly && !wardynScreenAsksStop && wardynScreenWindow != nil) {
			wardynScreenTeardown(); // the state goes to 0, which the Go side polls
		}
	});
}
- (void)unlocked:(NSNotification *)n {
	atomic_store(&wardynScreenLockLast, 0);
}
@end

static WardynScreenLockWatch *wardynScreenLockWatch; // main thread only; the center does not retain observers

// wardynScreenWatchLock observes the lock notifications on the main thread's run loop. They are delivered at once
// even while the app is inactive: AppKit suspends an inactive app's distributed notifications otherwise.
static void wardynScreenWatchLock(void) {
	wardynScreenLockWatch = [[WardynScreenLockWatch alloc] init];
	NSDistributedNotificationCenter *c = [NSDistributedNotificationCenter defaultCenter];
	[c addObserver:wardynScreenLockWatch
	      selector:@selector(locked:)
	          name:@"com.apple.screenIsLocked"
	        object:nil
	suspensionBehavior:NSNotificationSuspensionBehaviorDeliverImmediately];
	[c addObserver:wardynScreenLockWatch
	      selector:@selector(unlocked:)
	          name:@"com.apple.screenIsUnlocked"
	        object:nil
	suspensionBehavior:NSNotificationSuspensionBehaviorDeliverImmediately];
}

// wardynScreenLockNotices is the number of lock notifications seen; a window ends when it moves past its value
// at the open.
long wardynScreenLockNotices(void) { return atomic_load(&wardynScreenLockCount); }

// wardynScreenLockNoticed is 1 while the latest lock notification was a lock: the console check then refuses.
int wardynScreenLockNoticed(void) { return atomic_load(&wardynScreenLockLast); }

// wardynScreenShowWindow opens a run window on vm, a VZVirtualMachine the block keeps until the window closes:
// close-only (see WardynScreenCloseOnly), not floating, with title and steps (never NULL). A run window passes
// codeFile NULL (no Show code control), askStop 0 and canTrust 0. The edit window passes its setup-code file's
// path and askStop 1: its close button asks the guest to shut down (WardynScreenAskStop), a screen lock leaves
// it up, and its strip has the edit's buttons and live line; canTrust 0 disables Trust the list's CAs.
// At most one window: with one open it does nothing and the state stays as it was.
void wardynScreenShowWindow(void *vm, const char *title, const char *steps, const char *codeFile, int askStop, int canTrust) {
	VZVirtualMachine *machine = (__bridge VZVirtualMachine *)vm;
	NSString *t = [[NSString alloc] initWithUTF8String:title];
	NSString *s = [[NSString alloc] initWithUTF8String:steps];
	NSString *cf = codeFile == NULL ? nil : [[NSString alloc] initWithUTF8String:codeFile];
	BOOL edit = askStop != 0;
	BOOL trust = canTrust != 0;
	dispatch_async(dispatch_get_main_queue(), ^{
		if (wardynScreenWindow != nil) {
			return;
		}
		NSSize size = wardynScreenSize();
		NSWindowStyleMask mask = NSWindowStyleMaskTitled | NSWindowStyleMaskResizable | NSWindowStyleMaskClosable;
		NSWindow *w = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, size.width, size.height)
		                                          styleMask:mask
		                                            backing:NSBackingStoreBuffered
		                                              defer:NO];
		w.releasedWhenClosed = NO;
		w.title = t;
		if (edit) {
			wardynScreenStopper = [[WardynScreenAskStop alloc] init];
			w.delegate = wardynScreenStopper;
		} else {
			wardynScreenCloseOnly = [[WardynScreenCloseOnly alloc] init];
			w.delegate = wardynScreenCloseOnly;
		}
		[w addTitlebarAccessoryViewController:wardynScreenAccessory(size.width, s, cf, edit, trust)];
		w.contentAspectRatio = NSMakeSize(16, 9);
		VZVirtualMachineView *view = [[VZVirtualMachineView alloc] init];
		view.virtualMachine = machine;
		// The guest's display follows the window's size.
		view.automaticallyReconfiguresDisplay = YES;
		w.contentView = view;
		[w center];
		wardynScreenWindow = w;
		wardynScreenViewOnly = YES;
		wardynScreenAsksStop = edit;
		// An accessory app's window opens behind the active app, so it orders itself front regardless.
		[w makeKeyAndOrderFront:nil];
		[w orderFrontRegardless];
		[NSApp activate];
		atomic_store(&wardynScreenState, w.contentView != nil ? 1 : -1);
	});
}

// wardynScreenSetLive sets the edit window's live line to text (copied before this returns); a window without
// one ignores it.
void wardynScreenSetLive(const char *text) {
	NSString *t = [[NSString alloc] initWithUTF8String:text] ?: @"";
	dispatch_async(dispatch_get_main_queue(), ^{
		if (wardynScreenStrip.live != nil) {
			wardynScreenStrip.live.stringValue = t;
			[wardynScreenStrip fit];
		}
	});
}

// wardynScreenUp is wardynScreenState, for the Go side's wait.
int wardynScreenUp(void) { return atomic_load(&wardynScreenState); }

// wardynScreenClose closes the window and lets go of its VM.
void wardynScreenClose(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		wardynScreenTeardown();
	});
}
