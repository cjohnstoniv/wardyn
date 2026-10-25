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
#import <Virtualization/Virtualization.h>

static NSWindow *wardynScreenWindow; // main thread only

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
			w.contentAspectRatio = NSMakeSize(16, 9);
			VZVirtualMachineView *view = [[VZVirtualMachineView alloc] init];
			view.virtualMachine = machine;
			w.contentView = view;
			[w center];
			wardynScreenWindow = w;
		}
		[wardynScreenWindow makeKeyAndOrderFront:nil];
		[NSApp activate];
	});
}

// wardynScreenClose closes the window and lets go of its VM.
void wardynScreenClose(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[wardynScreenWindow orderOut:nil];
		wardynScreenWindow.contentView = nil;
		wardynScreenWindow = nil;
	});
}
