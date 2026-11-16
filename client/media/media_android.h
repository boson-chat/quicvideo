#ifndef QV_MEDIA_ANDROID_H
#define QV_MEDIA_ANDROID_H
#include <stdint.h>
#include <stddef.h>

// ---- camera (Camera2 NDK) ----
// JSON array of {id, name, kind, facing, orientation, formats:[{width,height,max_fps}]}.
char *qv_acam_list(char **err);
typedef struct qv_acam qv_acam;
qv_acam *qv_acam_start(const char *id, int width, int height, int fps, uintptr_t handle, char **err);
void qv_acam_stop(qv_acam *c);

// ---- encoder (AMediaCodec, H.264, NV12 input) ----
typedef struct qv_aenc qv_aenc;
qv_aenc *qv_aenc_new(int width, int height, int fps, int bitrate, int gop_seconds, uintptr_t handle, char **err);
// Encodes an NV12 picture (width*height luma, then interleaved UV).
int qv_aenc_encode(qv_aenc *e, const uint8_t *nv12, int64_t us, int force);
void qv_aenc_close(qv_aenc *e);

#endif
