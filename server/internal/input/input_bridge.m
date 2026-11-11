#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ApplicationServices/ApplicationServices.h>
#include <dlfcn.h>
#include "input_bridge.h"

DisplayBounds GetDisplayBounds(uint32_t displayID) {
    CGRect rect = CGDisplayBounds(displayID);
    DisplayBounds bounds;
    bounds.x = rect.origin.x;
    bounds.y = rect.origin.y;
    bounds.width = rect.size.width;
    bounds.height = rect.size.height;
    return bounds;
}

bool CheckAccessibilityPermission(void) {
    return AXIsProcessTrusted();
}

// Helper: warp cursor then post a mouse event with absolute positioning.
// Used for Down / Up events where the application needs a full event.
static void PostAbsoluteMouseEvent(CGEventType type, CGPoint point, CGMouseButton button) {
    CGWarpMouseCursorPosition(point);
    CGAssociateMouseAndMouseCursorPosition(true);
    CGEventRef event = CGEventCreateMouseEvent(NULL, type, point, button);
    if (event) {
        CGEventSetIntegerValueField(event, kCGMouseEventDeltaX, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventDeltaY, 0);
        CGEventPost(kCGHIDEventTap, event);
        CFRelease(event);
    }
}

// MouseMove needs a full CGEvent so that CGDisplayStream detects the cursor
// change and captures a new frame.  Pure CGWarp alone can cause missed frames.
void InjectMouseMove(double x, double y) {
    PostAbsoluteMouseEvent(kCGEventMouseMoved, CGPointMake(x, y), kCGMouseButtonLeft);
}

void InjectLeftMouseDown(double x, double y) {
    PostAbsoluteMouseEvent(kCGEventLeftMouseDown, CGPointMake(x, y), kCGMouseButtonLeft);
}

void InjectLeftMouseUp(double x, double y) {
    PostAbsoluteMouseEvent(kCGEventLeftMouseUp, CGPointMake(x, y), kCGMouseButtonLeft);
}

// Optimised drag: warp + CGEvent but skip CGAssociateMouseAndMouseCursorPosition.
// During continuous dragging we are in full control of cursor position, so the
// re-association call (one extra syscall per event) is unnecessary overhead.
void InjectLeftMouseDragged(double x, double y) {
    CGPoint point = CGPointMake(x, y);
    CGWarpMouseCursorPosition(point);
    CGEventRef event = CGEventCreateMouseEvent(NULL, kCGEventLeftMouseDragged, point, kCGMouseButtonLeft);
    if (event) {
        CGEventSetIntegerValueField(event, kCGMouseEventDeltaX, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventDeltaY, 0);
        CGEventPost(kCGHIDEventTap, event);
        CFRelease(event);
    }
}

void InjectRightMouseDown(double x, double y) {
    PostAbsoluteMouseEvent(kCGEventRightMouseDown, CGPointMake(x, y), kCGMouseButtonRight);
}

void InjectRightMouseUp(double x, double y) {
    PostAbsoluteMouseEvent(kCGEventRightMouseUp, CGPointMake(x, y), kCGMouseButtonRight);
}

void InjectScrollWheel(int32_t deltaX, int32_t deltaY) {
    CGEventRef event = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2, deltaY, deltaX);
    if (event) {
        CGEventPost(kCGHIDEventTap, event);
        CFRelease(event);
    }
}

void InjectTabletProximityEnter(void) {
    CGEventRef event = CGEventCreate(NULL);
    if (event) {
        CGEventSetType(event, kCGEventTabletProximity);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorID, 0x056A);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventTabletID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventPointerID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventDeviceID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventSystemTabletID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorPointerType, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorPointerSerialNumber, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorUniqueID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventCapabilityMask, 0x00FE);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventPointerType, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventEnterProximity, 1);
        CGEventPost(kCGHIDEventTap, event);
        CFRelease(event);
    }
}

void InjectTabletProximityLeave(void) {
    CGEventRef event = CGEventCreate(NULL);
    if (event) {
        CGEventSetType(event, kCGEventTabletProximity);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorID, 0x056A);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventTabletID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventPointerID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventDeviceID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventSystemTabletID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorPointerType, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorPointerSerialNumber, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventVendorUniqueID, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventCapabilityMask, 0x00FE);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventPointerType, 1);
        CGEventSetIntegerValueField(event, kCGTabletProximityEventEnterProximity, 0);
        CGEventPost(kCGHIDEventTap, event);
        CFRelease(event);
    }
}

// Tablet events: warp + CGEvent with tablet subtype.
// For Dragged events, skip CGAssociate (same optimisation as mouse drag).
static void InjectTabletEvent(CGEventType eventType, double x, double y, double pressure, double tiltX, double tiltY) {
    CGPoint point = CGPointMake(x, y);
    CGWarpMouseCursorPosition(point);
    // Only re-associate for Down/Up (not continuous Dragged/Moved)
    if (eventType == kCGEventLeftMouseDown || eventType == kCGEventLeftMouseUp) {
        CGAssociateMouseAndMouseCursorPosition(true);
    }
    CGEventRef event = CGEventCreateMouseEvent(NULL, eventType, point, kCGMouseButtonLeft);
    if (event) {
        CGEventSetIntegerValueField(event, kCGMouseEventDeltaX, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventDeltaY, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventSubtype, kCGEventMouseSubtypeTabletPoint);
        CGEventSetIntegerValueField(event, kCGTabletEventPointPressure, (int64_t)(pressure * 65535.0));
        CGEventSetDoubleValueField(event, kCGTabletEventTiltX, tiltX);
        CGEventSetDoubleValueField(event, kCGTabletEventTiltY, tiltY);
        CGEventPost(kCGHIDEventTap, event);
        CFRelease(event);
    }
}

void InjectTabletDown(double x, double y, double pressure, double tiltX, double tiltY) {
    InjectTabletEvent(kCGEventLeftMouseDown, x, y, pressure, tiltX, tiltY);
}

void InjectTabletUp(double x, double y, double pressure, double tiltX, double tiltY) {
    InjectTabletEvent(kCGEventLeftMouseUp, x, y, pressure, tiltX, tiltY);
}

void InjectTabletDragged(double x, double y, double pressure, double tiltX, double tiltY) {
    InjectTabletEvent(kCGEventLeftMouseDragged, x, y, pressure, tiltX, tiltY);
}

void InjectTabletMoved(double x, double y, double pressure, double tiltX, double tiltY) {
    InjectTabletEvent(kCGEventMouseMoved, x, y, pressure, tiltX, tiltY);
}

// --- Pointer events that do not use the hardware cursor -------------------------------------

bool WindowAt(double x, double y, int32_t *pid, uint32_t *windowID) {
    // Front-to-back order, on-screen windows only.
    CFArrayRef list = CGWindowListCopyWindowInfo(
        kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
    if (!list) {
        return false;
    }
    bool found = false;
    CGPoint point = CGPointMake(x, y);
    for (CFIndex i = 0; i < CFArrayGetCount(list) && !found; i++) {
        NSDictionary *info = (__bridge NSDictionary *)CFArrayGetValueAtIndex(list, i);
        // Layer 0 is where ordinary app windows live; menu bar, Dock and overlays use others.
        if ([info[(__bridge NSString *)kCGWindowLayer] intValue] != 0) {
            continue;
        }
        if ([info[(__bridge NSString *)kCGWindowAlpha] doubleValue] <= 0.0) {
            continue;
        }
        int32_t owner = [info[(__bridge NSString *)kCGWindowOwnerPID] intValue];
        CGRect bounds;
        if (!CGRectMakeWithDictionaryRepresentation(
                (__bridge CFDictionaryRef)info[(__bridge NSString *)kCGWindowBounds], &bounds)) {
            continue;
        }
        if (CGRectContainsPoint(bounds, point)) {
            *pid = owner;
            *windowID = [info[(__bridge NSString *)kCGWindowNumber] unsignedIntValue];
            found = true;
        }
    }
    CFRelease(list);
    return found;
}

// Marks which process and window an event is for. AppKit finds the NSWindow from the window
// number (field 51: without it the event arrives with no window and no view sees it), and
// WindowServer's hit-testing uses the "window under the pointer" fields, since no real
// pointer is over the window.
static const CGEventField kEventWindowNumber = (CGEventField)51;

// CGEventSetWindowLocation is private: the point inside the window that AppKit reports as
// locationInWindow. Without it the click arrives at the window's corner, outside its views.
// On macOS 26 (measured on 26.2) the stamp is taken as the window-local point, top-left origin:
// a screen point arrives as locationInWindow = (screen x, height - screen y). A project that
// does the same trick reports this starting at macOS 27 and a screen-point stamp before that,
// so the threshold is the first version seen to behave this way.
typedef void (*SetWindowLocationFn)(CGEventRef, CGPoint);

static SetWindowLocationFn WindowLocationSetter(void) {
    static SetWindowLocationFn fn = NULL;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        fn = (SetWindowLocationFn)dlsym(RTLD_DEFAULT, "CGEventSetWindowLocation");
    });
    return fn;
}

static CGPoint WindowOrigin(uint32_t windowID) {
    CGPoint origin = CGPointZero;
    CFArrayRef list = CGWindowListCopyWindowInfo(kCGWindowListOptionIncludingWindow, windowID);
    if (list && CFArrayGetCount(list) > 0) {
        NSDictionary *info = (__bridge NSDictionary *)CFArrayGetValueAtIndex(list, 0);
        CGRect bounds;
        if (CGRectMakeWithDictionaryRepresentation(
                (__bridge CFDictionaryRef)info[(__bridge NSString *)kCGWindowBounds], &bounds)) {
            origin = bounds.origin;
        }
    }
    if (list) {
        CFRelease(list);
    }
    return origin;
}

static void StampWindowLocation(CGEventRef event, uint32_t windowID, double x, double y) {
    SetWindowLocationFn set = WindowLocationSetter();
    if (!set) {
        return;
    }
    CGPoint point = CGPointMake(x, y);
    if ([[NSProcessInfo processInfo] operatingSystemVersion].majorVersion >= 26) {
        CGPoint origin = WindowOrigin(windowID);
        point = CGPointMake(x - origin.x, y - origin.y);
    }
    set(event, point);
}

static void SetTarget(CGEventRef event, int32_t pid, uint32_t windowID, double x, double y) {
    CGEventSetIntegerValueField(event, kCGEventTargetUnixProcessID, pid);
    CGEventSetIntegerValueField(event, kEventWindowNumber, windowID);
    CGEventSetIntegerValueField(event, kCGMouseEventWindowUnderMousePointer, windowID);
    CGEventSetIntegerValueField(event, kCGMouseEventWindowUnderMousePointerThatCanHandleThisEvent, windowID);
    StampWindowLocation(event, windowID, x, y);
}

// SkyLight's SLEventPostToPid is what WindowServer's own tools use to deliver an event to one
// process; it goes through the same activity bookkeeping as real input, which apps rely on to
// accept the event as live. It is private, so it is looked up at run time, with the public
// CGEventPostToPid as the fallback.
typedef void (*PostToPidFn)(pid_t, CGEventRef);

static PostToPidFn SkyLightPostToPid(void) {
    static PostToPidFn fn = NULL;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        void *handle = dlopen("/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight", RTLD_LAZY);
        if (handle) {
            fn = (PostToPidFn)dlsym(handle, "SLEventPostToPid");
        }
    });
    return fn;
}

static void PostEventToPid(int32_t pid, CGEventRef event) {
    PostToPidFn post = SkyLightPostToPid();
    if (post) {
        post((pid_t)pid, event);
    } else {
        CGEventPostToPid((pid_t)pid, event);
    }
}

void PostMouseToPid(int32_t pid, uint32_t windowID, int32_t type, int32_t button, double x, double y, int32_t clickState) {
    CGMouseButton cgButton = button == 1 ? kCGMouseButtonRight : kCGMouseButtonLeft;
    CGEventRef event = CGEventCreateMouseEvent(NULL, (CGEventType)type, CGPointMake(x, y), cgButton);
    if (!event) {
        return;
    }
    CGEventSetIntegerValueField(event, kCGMouseEventClickState, clickState);
    CGEventSetIntegerValueField(event, kCGMouseEventButtonNumber, button);
    CGEventSetIntegerValueField(event, kCGMouseEventDeltaX, 0);
    CGEventSetIntegerValueField(event, kCGMouseEventDeltaY, 0);
    SetTarget(event, pid, windowID, x, y);
    PostEventToPid(pid, event);
    CFRelease(event);
}

void PostScrollToPid(int32_t pid, uint32_t windowID, double x, double y, int32_t deltaX, int32_t deltaY) {
    CGEventRef event = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2, deltaY, deltaX);
    if (!event) {
        return;
    }
    CGEventSetLocation(event, CGPointMake(x, y));
    SetTarget(event, pid, windowID, x, y);
    PostEventToPid(pid, event);
    CFRelease(event);
}

void GetCursorPosition(double *x, double *y) {
    CGEventRef event = CGEventCreate(NULL);
    if (!event) {
        *x = 0;
        *y = 0;
        return;
    }
    CGPoint point = CGEventGetLocation(event);
    CFRelease(event);
    *x = point.x;
    *y = point.y;
}

void WarpCursor(double x, double y) {
    CGWarpMouseCursorPosition(CGPointMake(x, y));
    CGAssociateMouseAndMouseCursorPosition(true);
}

// SLPSPostEventRecordTo puts a raw event record in a process's queue. Two records tell an app
// that a window is now its focused one and its key one, without WindowServer raising the
// window or activating the app (the layout comes from the yabai window manager, which uses
// it to focus windows without raising them). All of it is private, so it is looked up at run
// time; if anything is missing this does nothing and clicks simply behave as before.
typedef int32_t (*PostEventRecordFn)(const void *psn, const uint8_t *record);
typedef int32_t (*GetProcessForPIDFn)(pid_t pid, void *psn);

static void FillWindowRecord(uint8_t record[0xF8], uint8_t kind, uint32_t windowID) {
    memset(record, 0, 0xF8);
    record[0x04] = 0xF8;
    record[0x08] = kind;
    memcpy(record + 0x3C, &windowID, sizeof(windowID)); // little-endian on every Mac
}

void PrimeWindowForInput(int32_t pid, uint32_t windowID) {
    static PostEventRecordFn post = NULL;
    static GetProcessForPIDFn processForPID = NULL;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        void *skylight = dlopen("/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight", RTLD_LAZY);
        if (skylight) {
            post = (PostEventRecordFn)dlsym(skylight, "SLPSPostEventRecordTo");
        }
        processForPID = (GetProcessForPIDFn)dlsym(RTLD_DEFAULT, "GetProcessForPID");
    });
    if (!post || !processForPID) {
        return;
    }
    uint8_t psn[8] = {0};
    if (processForPID((pid_t)pid, psn) != 0) {
        return;
    }

    uint8_t record[0xF8];
    FillWindowRecord(record, 0x0D, windowID); // focus
    record[0x8A] = 0x01;
    post(psn, record);
    usleep(40 * 1000); // the app handles these on its main thread; give it a moment between them

    for (uint8_t kind = 1; kind <= 2; kind++) { // make-key press / release
        FillWindowRecord(record, kind, windowID);
        record[0x3A] = 0x10;
        memset(record + 0x20, 0xFF, 0x10);
        post(psn, record);
    }
    usleep(20 * 1000);
}
