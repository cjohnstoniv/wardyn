// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

// The one first boot of a macOS base, provisioned or plain (see provision.go).
//
// VZMacGuestProvisioningOptions and the validated start-option setter are
// macOS 27 API. They are looked up at run time so this file builds against
// the macOS 26 SDK. Every class and selector looked up here must appear in
// scripts/check-macvm-selectors.sh, which also checks them against the
// macOS 27 SDK headers.

#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>
#import <objc/message.h>
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static char *wardynCopyString(NSString *s) {
	return strdup(s.UTF8String ?: "unknown error");
}

static char *wardynCopyError(NSError *err) {
	return wardynCopyString(err.localizedDescription);
}

// A start error's text. The framework's VM limit refusal (VZErrorDomain code 6, macvm.IsVMLimit) starts with
// "vm-limit: " (provisionVMLimitMark in provision.go), which is how the Go side tells it apart.
static char *wardynCopyStartError(NSError *err) {
	if ([err.domain isEqualToString:@"VZErrorDomain"] && err.code == 6) {
		return wardynCopyString([@"vm-limit: " stringByAppendingString:err.localizedDescription ?: @"unknown error"]);
	}
	return wardynCopyError(err);
}

// The name of the first class or selector this host lacks, or nil.
static NSString *wardynMissing(void) {
	if (!VZVirtualMachine.isSupported) {
		return @"virtualization support";
	}
	Class cls = NSClassFromString(@"VZMacGuestProvisioningOptions");
	if (cls == Nil) {
		return @"class VZMacGuestProvisioningOptions";
	}
	SEL setters[] = {
		@selector(setUsername:),
		@selector(setPassword:),
		@selector(setFullName:),
		@selector(setLogsInAutomatically:),
		@selector(setEnablesRemoteLogin:),
	};
	for (size_t i = 0; i < sizeof(setters) / sizeof(setters[0]); i++) {
		if (![cls instancesRespondToSelector:setters[i]]) {
			return [NSString stringWithFormat:@"selector -[VZMacGuestProvisioningOptions %@]", NSStringFromSelector(setters[i])];
		}
	}
	if (![VZMacOSVirtualMachineStartOptions instancesRespondToSelector:@selector(setGuestProvisioningOptions:error:)]) {
		return @"selector -[VZMacOSVirtualMachineStartOptions setGuestProvisioningOptions:error:]";
	}
	return nil;
}

char *wardynProvisionMissing(void) {
	@autoreleasepool {
		NSString *m = wardynMissing();
		return m == nil ? NULL : wardynCopyString(m);
	}
}

@interface WardynProvisionVM : NSObject <VZVirtualMachineDelegate>
@property(strong) VZVirtualMachine *vm;
@property(strong) dispatch_queue_t queue;
@property(strong) dispatch_semaphore_t stopped;
@property(copy) NSString *stopError;
@property BOOL finished;
@end

@implementation WardynProvisionVM

// finish runs on the VM's queue; the first call wins.
- (void)finish:(NSError *)err {
	if (self.finished) {
		return;
	}
	self.finished = YES;
	self.stopError = err.localizedDescription;
	dispatch_semaphore_signal(self.stopped);
}

- (void)guestDidStopVirtualMachine:(VZVirtualMachine *)vm {
	[self finish:nil];
}

- (void)virtualMachine:(VZVirtualMachine *)vm didStopWithError:(NSError *)err {
	[self finish:err];
}

@end

static VZVirtualMachineConfiguration *wardynConfig(const char *disk, const char *aux,
                                                   const void *hw, int hwLen, const void *mid, int midLen,
                                                   unsigned cpus, unsigned long long mem, int fd, NSString **errOut) {
	VZMacHardwareModel *model = [[VZMacHardwareModel alloc] initWithDataRepresentation:[NSData dataWithBytes:hw length:hwLen]];
	if (model == nil || !model.supported) {
		*errOut = @"the base's hardware model is not supported on this Mac";
		return nil;
	}
	VZMacMachineIdentifier *ident = [[VZMacMachineIdentifier alloc] initWithDataRepresentation:[NSData dataWithBytes:mid length:midLen]];
	if (ident == nil) {
		*errOut = @"the base's machine identifier is not valid";
		return nil;
	}
	VZMacPlatformConfiguration *platform = [[VZMacPlatformConfiguration alloc] init];
	platform.hardwareModel = model;
	platform.machineIdentifier = ident;
	platform.auxiliaryStorage = [[VZMacAuxiliaryStorage alloc] initWithURL:[NSURL fileURLWithPath:@(aux)]];

	NSError *err = nil;
	VZDiskImageStorageDeviceAttachment *diskAttachment =
	    [[VZDiskImageStorageDeviceAttachment alloc] initWithURL:[NSURL fileURLWithPath:@(disk)] readOnly:NO error:&err];
	if (diskAttachment == nil) {
		*errOut = [@"open disk image: " stringByAppendingString:err.localizedDescription];
		return nil;
	}

	int nicFD = fcntl(fd, F_DUPFD_CLOEXEC, 0); // close-on-exec: the helper runs pmset while this VM runs
	if (nicFD < 0) {
		*errOut = [NSString stringWithFormat:@"duplicate the network socket: %s", strerror(errno)];
		return nil;
	}
	NSFileHandle *nicHandle = [[NSFileHandle alloc] initWithFileDescriptor:nicFD closeOnDealloc:YES];
	VZVirtioNetworkDeviceConfiguration *nic = [[VZVirtioNetworkDeviceConfiguration alloc] init];
	nic.attachment = [[VZFileHandleNetworkDeviceAttachment alloc] initWithFileHandle:nicHandle];

	VZMacGraphicsDeviceConfiguration *graphics = [[VZMacGraphicsDeviceConfiguration alloc] init];
	graphics.displays = @[ [[VZMacGraphicsDisplayConfiguration alloc] initWithWidthInPixels:1920 heightInPixels:1080 pixelsPerInch:80] ];

	VZVirtualMachineConfiguration *cfg = [VZVirtualMachineConfiguration new];
	cfg.platform = platform;
	cfg.bootLoader = [[VZMacOSBootLoader alloc] init];
	cfg.CPUCount = cpus;
	cfg.memorySize = mem;
	cfg.storageDevices = @[ [[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:diskAttachment] ];
	cfg.networkDevices = @[ nic ];
	cfg.graphicsDevices = @[ graphics ];
	cfg.keyboards = @[ [[VZUSBKeyboardConfiguration alloc] init] ];
	cfg.pointingDevices = @[ [[VZUSBScreenCoordinatePointingDeviceConfiguration alloc] init] ];
	if (![cfg validateWithError:&err]) {
		*errOut = [@"invalid VM configuration: " stringByAppendingString:err.localizedDescription];
		return nil;
	}
	return cfg;
}

// The start options with guest provisioning set through the validated setter.
static VZMacOSVirtualMachineStartOptions *wardynStartOptions(const char *user, const char *pass, NSString **errOut) {
	NSString *missing = wardynMissing();
	if (missing != nil) {
		*errOut = [@"no " stringByAppendingString:missing];
		return nil;
	}
	NSString *username = @(user);
	id prov = [NSClassFromString(@"VZMacGuestProvisioningOptions") new];
	((void (*)(id, SEL, id))objc_msgSend)(prov, @selector(setUsername:), username);
	((void (*)(id, SEL, id))objc_msgSend)(prov, @selector(setPassword:), @(pass));
	((void (*)(id, SEL, id))objc_msgSend)(prov, @selector(setFullName:), username);
	((void (*)(id, SEL, BOOL))objc_msgSend)(prov, @selector(setLogsInAutomatically:), YES);
	((void (*)(id, SEL, BOOL))objc_msgSend)(prov, @selector(setEnablesRemoteLogin:), YES);

	VZMacOSVirtualMachineStartOptions *opts = [VZMacOSVirtualMachineStartOptions new];
	NSError *err = nil;
	BOOL ok = ((BOOL (*)(id, SEL, id, NSError *__autoreleasing *))objc_msgSend)(
	    opts, @selector(setGuestProvisioningOptions:error:), prov, &err);
	if (!ok) {
		*errOut = [@"guest provisioning options refused: " stringByAppendingString:err.localizedDescription ?: @"no reason given"];
		return nil;
	}
	return opts;
}

// wardynProvisionStart builds and starts the VM, blocking until the start
// completes. An empty user starts it with no provisioning options, for a
// setup by hand. It returns a retained handle, or NULL with *errOut set (free
// it).
void *wardynProvisionStart(const char *disk, const char *aux, const void *hw, int hwLen, const void *mid, int midLen,
                           unsigned cpus, unsigned long long mem, int fd, const char *user, const char *pass, char **errOut) {
	@autoreleasepool {
		NSString *msg = nil;
		VZMacOSVirtualMachineStartOptions *opts =
		    user[0] == '\0' ? [VZMacOSVirtualMachineStartOptions new] : wardynStartOptions(user, pass, &msg);
		if (opts == nil) {
			*errOut = wardynCopyString(msg);
			return NULL;
		}
		VZVirtualMachineConfiguration *cfg = wardynConfig(disk, aux, hw, hwLen, mid, midLen, cpus, mem, fd, &msg);
		if (cfg == nil) {
			*errOut = wardynCopyString(msg);
			return NULL;
		}

		WardynProvisionVM *h = [WardynProvisionVM new];
		h.queue = dispatch_queue_create("com.wardyn.macvm.provision", DISPATCH_QUEUE_SERIAL);
		h.stopped = dispatch_semaphore_create(0);
		dispatch_sync(h.queue, ^{
			h.vm = [[VZVirtualMachine alloc] initWithConfiguration:cfg queue:h.queue];
			h.vm.delegate = h;
		});

		__block NSError *startErr = nil;
		dispatch_semaphore_t started = dispatch_semaphore_create(0);
		dispatch_async(h.queue, ^{
			[h.vm startWithOptions:opts
			     completionHandler:^(NSError *err) {
				     startErr = err;
				     dispatch_semaphore_signal(started);
			     }];
		});
		dispatch_semaphore_wait(started, DISPATCH_TIME_FOREVER);
		if (startErr != nil) {
			*errOut = wardynCopyStartError(startErr);
			return NULL;
		}
		return (void *)CFBridgingRetain(h);
	}
}

// wardynProvisionWait blocks until the VM stops. It returns NULL when the
// guest shut itself down or the host stopped it, else the error (free it).
char *wardynProvisionWait(void *p) {
	WardynProvisionVM *h = (__bridge WardynProvisionVM *)p;
	dispatch_semaphore_wait(h.stopped, DISPATCH_TIME_FOREVER);
	@autoreleasepool {
		NSString *msg = h.stopError;
		return msg == nil ? NULL : wardynCopyString(msg);
	}
}

// wardynProvisionRequestStop asks the guest to shut down; a refusal is
// covered by the caller's force stop.
void wardynProvisionRequestStop(void *p) {
	WardynProvisionVM *h = (__bridge WardynProvisionVM *)p;
	dispatch_sync(h.queue, ^{
		if (!h.finished && h.vm.canRequestStop) {
			[h.vm requestStopWithError:nil];
		}
	});
}

// wardynProvisionForceStop stops the VM without asking the guest. When no
// stop is possible the VM is already stopping, and the delegate finishes it.
void wardynProvisionForceStop(void *p) {
	WardynProvisionVM *h = (__bridge WardynProvisionVM *)p;
	dispatch_sync(h.queue, ^{
		if (!h.finished && h.vm.canStop) {
			[h.vm stopWithCompletionHandler:^(NSError *err) {
				[h finish:err];
			}];
		}
	});
}

void wardynProvisionFree(void *p) {
	CFBridgingRelease(p);
}

// wardynProvisionMachine is the handle's VM, unretained, for its window
// (screen_darwin.m). The caller holds the handle while it uses it.
void *wardynProvisionMachine(void *p) {
	WardynProvisionVM *h = (__bridge WardynProvisionVM *)p;
	return (__bridge void *)h.vm;
}
