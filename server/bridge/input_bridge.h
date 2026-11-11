#ifndef INPUT_BRIDGE_H
#define INPUT_BRIDGE_H

#include <stdint.h>
#include <stdbool.h>

typedef struct {
    double x;
    double y;
    double width;
    double height;
} DisplayBounds;

DisplayBounds GetDisplayBounds(uint32_t displayID);
bool CheckAccessibilityPermission(void);

void InjectMouseMove(double x, double y);
void InjectLeftMouseDown(double x, double y);
void InjectLeftMouseUp(double x, double y);
void InjectLeftMouseDragged(double x, double y);
void InjectRightMouseDown(double x, double y);
void InjectRightMouseUp(double x, double y);
void InjectScrollWheel(int32_t deltaX, int32_t deltaY);

void InjectTabletProximityEnter(void);
void InjectTabletProximityLeave(void);
void InjectTabletDown(double x, double y, double pressure, double tiltX, double tiltY);
void InjectTabletUp(double x, double y, double pressure, double tiltX, double tiltY);
void InjectTabletDragged(double x, double y, double pressure, double tiltX, double tiltY);
void InjectTabletMoved(double x, double y, double pressure, double tiltX, double tiltY);

// --- Pointer events that do not use the hardware cursor -------------------------------------

// Finds the topmost normal window under (x, y) in global coordinates. Returns false when there
// is none (desktop, Dock, menu bar, ...).
bool WindowAt(double x, double y, int32_t *pid, uint32_t *windowID);

// Posts a mouse event only to the process owning windowID; the cursor is not touched.
// type is a CGEventType value (mouse moved/down/up/dragged), button 0 = left, 1 = right.
void PostMouseToPid(int32_t pid, uint32_t windowID, int32_t type, int32_t button, double x, double y, int32_t clickState);

// Posts a pixel scroll event only to the process owning windowID, located at (x, y).
void PostScrollToPid(int32_t pid, uint32_t windowID, double x, double y, int32_t deltaX, int32_t deltaY);

void GetCursorPosition(double *x, double *y);
void WarpCursor(double x, double y);

// Makes windowID the key window of its app without raising it or activating the app, so the
// app's views accept mouse events posted to it. Needed because an inactive app ignores them.
void PrimeWindowForInput(int32_t pid, uint32_t windowID);

#endif
