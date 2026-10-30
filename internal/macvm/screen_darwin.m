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
// The window has no close button: it closes when setup by hand ends.

#import <AppKit/AppKit.h>
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

// wardynScreenRun starts AppKit on the main thread. It does not return.
void wardynScreenRun(void) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
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

// wardynScreenAccessory is the strip of steps under the window's title bar.
static NSTitlebarAccessoryViewController *wardynScreenAccessory(CGFloat width) {
	NSTextField *label = [NSTextField wrappingLabelWithString:wardynScreenSteps];
	label.font = [NSFont systemFontOfSize:12];
	// The strip is as tall as the steps are at this width (a narrow window wraps them).
	NSSize fit = [label.cell cellSizeForBounds:NSMakeRect(0, 0, width - 20, CGFLOAT_MAX)];
	NSView *box = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, width, ceil(fit.height) + 8)];
	label.frame = NSInsetRect(box.bounds, 10, 4);
	label.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
	[box addSubview:label];
	NSTitlebarAccessoryViewController *a = [[NSTitlebarAccessoryViewController alloc] init];
	a.layoutAttribute = NSLayoutAttributeBottom;
	a.view = box;
	return a;
}

// wardynScreenShow opens the window on vm, a VZVirtualMachine; the block
// keeps it until the window closes.
void wardynScreenShow(void *vm) {
	VZVirtualMachine *machine = (__bridge VZVirtualMachine *)vm;
	dispatch_async(dispatch_get_main_queue(), ^{
		if (wardynScreenWindow == nil) {
			NSSize size = wardynScreenSize();
			NSWindow *w = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, size.width, size.height)
			                                          styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskResizable
			                                            backing:NSBackingStoreBuffered
			                                              defer:NO];
			w.releasedWhenClosed = NO;
			w.title = @"Wardyn: set up this macOS VM";
			[w addTitlebarAccessoryViewController:wardynScreenAccessory(size.width)];
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

// wardynScreenUp is wardynScreenState, for the Go side's wait.
int wardynScreenUp(void) { return atomic_load(&wardynScreenState); }

// wardynScreenClose closes the window and lets go of its VM.
void wardynScreenClose(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[wardynScreenWindow orderOut:nil];
		wardynScreenWindow.contentView = nil;
		wardynScreenWindow = nil;
		atomic_store(&wardynScreenState, 0);
	});
}
