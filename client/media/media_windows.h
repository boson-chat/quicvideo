// C side of package media on Windows: Media Foundation (H.264 encode and
// decode MFTs, webcams) and Windows.Graphics.Capture (screens, windows).
#ifndef QV_MEDIA_WINDOWS_H
#define QV_MEDIA_WINDOWS_H

#include <stdint.h>

// A picture: top-down BGRA in C memory.
typedef struct {
    uint8_t *data;
    int w, h, stride;
} qv_wpic;

qv_wpic *qv_wpic_new(int w, int h);
void qv_wpic_free(qv_wpic *p);

// Conversions (BT.601 limited range), with bilinear scaling.
void qv_bgra_to_nv12(const uint8_t *src, int sw, int sh, int sstride, uint8_t *y, int ystride, uint8_t *uv,
                     int uvstride, int dw, int dh);
void qv_nv12_to_bgra(const uint8_t *y, int ystride, const uint8_t *uv, int uvstride, int w, int h, uint8_t *dst,
                     int dstride);

// ---- Media Foundation ----
int qv_mf_startup(char **err);

typedef struct qv_wenc qv_wenc;
// prefer_hw: try hardware MFTs first. name gets the MFT's friendly name.
qv_wenc *qv_wenc_new(int w, int h, int fps, int bitrate, int gop, int screen, int prefer_hw, uintptr_t handle,
                     char **err, char *name, int name_len);
int qv_wenc_encode(qv_wenc *e, qv_wpic *p, int64_t us, int force);
void qv_wenc_close(qv_wenc *e);

typedef struct qv_wdec qv_wdec;
qv_wdec *qv_wdec_new(uintptr_t handle, char **err);
int qv_wdec_decode(qv_wdec *d, const uint8_t *data, int len, int64_t us);
void qv_wdec_close(qv_wdec *d);

char *qv_cam_list(char **err); // lines: "symlink\tname"
typedef struct qv_wcam qv_wcam;
// Returns NULL + err; *denied=1 when Windows privacy settings block cameras.
qv_wcam *qv_cam_start(const char *symlink, int w, int h, int fps, uintptr_t handle, char **err, int *denied);
void qv_cam_stop(qv_wcam *c);

// ---- Windows.Graphics.Capture ----
char *qv_wgc_list(void); // lines: "D\tid\tw\th\tprimary" and "W\thwnd\tw\th\ttitle"
int qv_wgc_supported(void);
typedef struct qv_wgc qv_wgc;
// monitor index (0 = primary) or hwnd (when nonzero). borderless: ask
// Windows 11 not to draw its capture border (thumbnails).
qv_wgc *qv_wgc_start(int monitor, uintptr_t hwnd, int fps, int cursor, int borderless, uintptr_t handle, char **err);
void qv_wgc_stop(qv_wgc *c);

#endif
