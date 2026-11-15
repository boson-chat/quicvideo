// C interface between the Go side of package media and the Objective-C
// implementations (VideoToolbox, ScreenCaptureKit, AVFoundation).
#ifndef QV_MEDIA_DARWIN_H
#define QV_MEDIA_DARWIN_H

#include <stdint.h>
#include <CoreVideo/CoreVideo.h>

// ---- VideoToolbox encoder ----
typedef struct qv_enc qv_enc;
// Returns NULL and sets *err (malloc'd, caller frees) on failure.
qv_enc *qv_enc_new(int width, int height, int fps, int bitrate, int gop_frames, int screen,
                   uintptr_t handle, char **err, int *hardware);
int qv_enc_encode(qv_enc *e, void *pb, int64_t us, int force);
void qv_enc_close(qv_enc *e);

// ---- VideoToolbox decoder ----
typedef struct qv_dec qv_dec;
qv_dec *qv_dec_new(uintptr_t handle);
// Returns 0 on success; sps/pps may be NULL (keep the current format).
int qv_dec_decode(qv_dec *d, const void *sps, int sps_len, const void *pps, int pps_len,
                  const void *avcc, int len, int64_t us);
void qv_dec_close(qv_dec *d);

// ---- pixel buffers (void * = CVPixelBufferRef: cgo maps the typedef
// inconsistently, so it stays opaque on the Go side) ----
void *qv_pb_new_bgra(int width, int height); // IOSurface-backed, refcount 1
int qv_pb_format(void *pb);                  // FourCC
int qv_pb_width(void *pb);
int qv_pb_height(void *pb);
void qv_pb_retain(void *pb);
void qv_pb_release(void *pb);
void *qv_pb_iosurface(void *pb);
void *qv_pb_lock(void *pb, int readonly, int *stride); // base address
void qv_pb_unlock(void *pb, int readonly);

// ---- permissions (TCC) ----
// 1 granted, 0 denied/restricted, -1 not determined (a prompt would show).
int qv_screen_access(void);
void qv_screen_request(void); // shows the prompt once; the answer applies after a restart
int qv_camera_access(void);
// Blocks up to timeout_ms waiting for the user; returns qv_camera_access().
int qv_camera_request(int timeout_ms);

// ---- ScreenCaptureKit ----
// Fills JSON (malloc'd) describing displays and windows; returns NULL + err.
char *qv_screen_list(char **err);
typedef struct qv_screen qv_screen;
// display_id != 0 captures a display; else window_id. width/height 0 = native.
qv_screen *qv_screen_start(uint32_t display_id, uint32_t window_id, int width, int height, int fps,
                           int show_cursor, uintptr_t handle, char **err);
void qv_screen_stop(qv_screen *s);

// ---- AVFoundation camera ----
char *qv_camera_list(char **err);
typedef struct qv_camera qv_camera;
qv_camera *qv_camera_start(const char *unique_id, int width, int height, int fps, uintptr_t handle, char **err);
void qv_camera_stop(qv_camera *c);

#endif
