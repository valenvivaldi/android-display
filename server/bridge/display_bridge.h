#ifndef DISPLAY_BRIDGE_H
#define DISPLAY_BRIDGE_H

#include <CoreGraphics/CoreGraphics.h>

typedef struct {
    int width;
    int height;
    int ppi;
    int hiDPI;
    unsigned int serial; // identity of the display; macOS keys its saved arrangement on it
    char name[64];       // shown in System Settings → Displays; empty = default
} VirtualDisplayConfig;

typedef struct {
    void *display;
    CGDirectDisplayID displayID;
    int success;
    char errorMsg[256];
} VirtualDisplayResult;

VirtualDisplayResult CreateVirtualDisplay(VirtualDisplayConfig config);
void DestroyVirtualDisplay(void *display);
CGDirectDisplayID GetVirtualDisplayID(void *display);

#endif
