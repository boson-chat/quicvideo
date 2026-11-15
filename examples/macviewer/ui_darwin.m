// Minimal AppKit UI for macviewer: one window, a CALayer per remote track
// showing decoded IOSurfaces, a label, click to pin.
#import <AppKit/AppKit.h>
#import <CoreVideo/CoreVideo.h>
#import <QuartzCore/QuartzCore.h>
#include "ui_darwin.h"

extern void qvGoUIResize(double w, double h, double scale);
extern void qvGoUIClick(uint32_t track_id);
extern void qvGoUIVisible(int visible);
extern void qvGoUIClosed(void);

@interface QVTileLayer : CALayer
@property(nonatomic) uint32_t trackID;
@property(nonatomic, strong) CATextLayer *label;
@property(nonatomic) CVPixelBufferRef buffer; // retained
@end
@implementation QVTileLayer
- (void)dealloc {
    if (_buffer) CVPixelBufferRelease(_buffer);
}
@end

@interface QVView : NSView
@property(nonatomic, strong) NSMutableDictionary<NSNumber *, QVTileLayer *> *tiles;
@end

@implementation QVView
- (BOOL)isFlipped {
    return YES;
}
- (void)mouseDown:(NSEvent *)event {
    NSPoint p = [self convertPoint:event.locationInWindow fromView:nil];
    for (QVTileLayer *t in self.tiles.allValues) {
        if (!t.hidden && CGRectContainsPoint(t.frame, p)) {
            qvGoUIClick(t.trackID);
            return;
        }
    }
}
@end

@interface QVDelegate : NSObject <NSApplicationDelegate, NSWindowDelegate>
@property(nonatomic, strong) NSWindow *window;
@property(nonatomic, strong) QVView *view;
@end

static QVDelegate *gDelegate;

static void reportSize(void) {
    NSRect b = gDelegate.view.bounds;
    qvGoUIResize(b.size.width, b.size.height, gDelegate.window.backingScaleFactor);
}

@implementation QVDelegate
- (void)applicationDidFinishLaunching:(NSNotification *)n {
    [NSApp activateIgnoringOtherApps:YES];
    reportSize();
}
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)app {
    return YES;
}
- (void)windowDidResize:(NSNotification *)n {
    reportSize();
}
- (void)windowDidChangeBackingProperties:(NSNotification *)n {
    reportSize();
}
- (void)windowDidChangeOcclusionState:(NSNotification *)n {
    qvGoUIVisible((self.window.occlusionState & NSWindowOcclusionStateVisible) ? 1 : 0);
}
- (void)windowDidMiniaturize:(NSNotification *)n {
    qvGoUIVisible(0);
}
- (void)windowDidDeminiaturize:(NSNotification *)n {
    qvGoUIVisible(1);
}
- (void)windowWillClose:(NSNotification *)n {
    qvGoUIClosed();
}
@end

void qv_ui_run(const char *title, int width, int height) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
        gDelegate = [[QVDelegate alloc] init];
        NSWindow *w = [[NSWindow alloc]
            initWithContentRect:NSMakeRect(0, 0, width, height)
                      styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskResizable |
                                NSWindowStyleMaskMiniaturizable
                        backing:NSBackingStoreBuffered
                          defer:NO];
        w.title = [NSString stringWithUTF8String:title];
        w.backgroundColor = [NSColor colorWithCalibratedRed:0.06 green:0.07 blue:0.08 alpha:1];
        QVView *v = [[QVView alloc] initWithFrame:w.contentView.bounds];
        v.wantsLayer = YES;
        v.layer.backgroundColor = w.backgroundColor.CGColor;
        v.tiles = [NSMutableDictionary dictionary];
        w.contentView = v;
        w.delegate = gDelegate;
        [w center];
        [w makeKeyAndOrderFront:nil];
        gDelegate.window = w;
        gDelegate.view = v;
        NSApp.delegate = gDelegate;
        [NSApp run];
    }
}

void qv_ui_quit(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
      [NSApp terminate:nil];
    });
}

void qv_ui_set_tiles(int n, const uint32_t *ids, const double *rects, const char **labels, const int *visible) {
    // Copy: the arguments are freed when this returns.
    NSMutableArray *items = [NSMutableArray array];
    for (int i = 0; i < n; i++) {
        [items addObject:@{
            @"id" : @(ids[i]),
            @"rect" : [NSValue valueWithRect:NSMakeRect(rects[4 * i], rects[4 * i + 1], rects[4 * i + 2], rects[4 * i + 3])],
            @"label" : [NSString stringWithUTF8String:labels[i]],
            @"visible" : @(visible[i])
        }];
    }
    dispatch_async(dispatch_get_main_queue(), ^{
      QVView *v = gDelegate.view;
      if (v == nil) return;
      [CATransaction begin];
      [CATransaction setDisableActions:YES];
      NSMutableSet *keep = [NSMutableSet set];
      for (NSDictionary *it in items) {
          NSNumber *key = it[@"id"];
          [keep addObject:key];
          QVTileLayer *t = v.tiles[key];
          if (t == nil) {
              t = [QVTileLayer layer];
              t.trackID = key.unsignedIntValue;
              t.backgroundColor = [NSColor colorWithCalibratedWhite:0.12 alpha:1].CGColor;
              t.cornerRadius = 8;
              t.masksToBounds = YES;
              t.contentsGravity = kCAGravityResizeAspect;
              t.contentsScale = gDelegate.window.backingScaleFactor;
              CATextLayer *l = [CATextLayer layer];
              l.fontSize = 12;
              l.foregroundColor = NSColor.whiteColor.CGColor;
              l.backgroundColor = [NSColor colorWithCalibratedWhite:0 alpha:0.55].CGColor;
              l.contentsScale = gDelegate.window.backingScaleFactor;
              l.cornerRadius = 4;
              t.label = l;
              [t addSublayer:l];
              [v.layer addSublayer:t];
              v.tiles[key] = t;
          }
          t.frame = [it[@"rect"] rectValue];
          t.hidden = ![it[@"visible"] boolValue];
          t.label.string = it[@"label"];
          t.label.frame = CGRectMake(8, 8, MIN(t.frame.size.width - 16, 7.2 * [it[@"label"] length] + 12), 18);
      }
      for (NSNumber *key in v.tiles.allKeys) {
          if (![keep containsObject:key]) {
              [v.tiles[key] removeFromSuperlayer];
              [v.tiles removeObjectForKey:key];
          }
      }
      [CATransaction commit];
    });
}

void qv_ui_set_frame(uint32_t track_id, void *pb) {
    CVPixelBufferRef buf = (CVPixelBufferRef)pb;
    CVPixelBufferRetain(buf);
    dispatch_async(dispatch_get_main_queue(), ^{
      QVTileLayer *t = gDelegate.view.tiles[@(track_id)];
      if (t == nil || t.hidden) {
          CVPixelBufferRelease(buf);
          return;
      }
      [CATransaction begin];
      [CATransaction setDisableActions:YES];
      t.contents = (__bridge id)CVPixelBufferGetIOSurface(buf);
      [CATransaction commit];
      if (t.buffer) CVPixelBufferRelease(t.buffer);
      t.buffer = buf; // keep the surface alive while shown
    });
}

int qv_ui_window_id(void) {
    __block int wid = 0;
    dispatch_sync(dispatch_get_main_queue(), ^{
      wid = (int)gDelegate.window.windowNumber;
    });
    return wid;
}
