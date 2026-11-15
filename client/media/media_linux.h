// C side of package media on Linux: libavcodec (NVENC, VA-API, x264 opt-in,
// H.264 decode), openh264 (software encode), libswscale, libavdevice (V4L2,
// x11grab) and PipeWire (portal screen capture).
#ifndef QV_MEDIA_LINUX_H
#define QV_MEDIA_LINUX_H

#include <stdint.h>
#include <libavutil/frame.h>

// ---- frames ----
AVFrame *qv_frame_new_bgra(int w, int h);
void qv_frame_free(AVFrame *f);
int qv_frame_w(AVFrame *f);
int qv_frame_h(AVFrame *f);
int qv_frame_format(AVFrame *f);
// Converts/scales to a new RGBA frame (w,h); NULL on failure.
AVFrame *qv_frame_to_rgba(AVFrame *src, int w, int h);
uint8_t *qv_frame_data(AVFrame *f, int plane, int *linesize);

// ---- encoder ----
enum { QV_ENC_AUTO = 0, QV_ENC_NVENC = 1, QV_ENC_VAAPI = 2, QV_ENC_OPENH264 = 3, QV_ENC_X264 = 4 };
typedef struct qv_lenc qv_lenc;
// backend: QV_ENC_*; allow_gpl lets AUTO use libx264. name gets the backend used.
qv_lenc *qv_lenc_new(int w, int h, int fps, int bitrate, int gop, int screen, int backend, int allow_gpl,
                     uintptr_t handle, char **err, char *name, int name_len);
int qv_lenc_encode(qv_lenc *e, AVFrame *src, int64_t us, int force);
void qv_lenc_close(qv_lenc *e);

// ---- decoder ----
typedef struct qv_ldec qv_ldec;
qv_ldec *qv_ldec_new(uintptr_t handle, char **err);
int qv_ldec_decode(qv_ldec *d, const uint8_t *data, int len, int64_t us);
void qv_ldec_close(qv_ldec *d);

// ---- capture: libavdevice (V4L2 camera, X11 screen) ----
typedef struct qv_avcap qv_avcap;
// fmt: "video4linux2" or "x11grab"; url: /dev/videoN or ":0.0+x,y".
qv_avcap *qv_avcap_start(const char *fmt, const char *url, int w, int h, int fps, uintptr_t handle, char **err);
void qv_avcap_stop(qv_avcap *c);
char *qv_v4l2_list(void); // newline-separated "path\tname"

// ---- capture: PipeWire stream from the ScreenCast portal ----
typedef struct qv_pwcap qv_pwcap;
qv_pwcap *qv_pwcap_start(int fd, uint32_t node, int max_w, int fps, uintptr_t handle, char **err);
void qv_pwcap_stop(qv_pwcap *c);

#endif
