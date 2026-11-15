//go:build windows && cgo

// Pixel conversions for the Windows backend (Media Foundation's H.264 MFTs
// take and give NV12; captures and the UI use BGRA).
#include <stdlib.h>
#include <string.h>
#include "media_windows.h"

qv_wpic *qv_wpic_new(int w, int h) {
    qv_wpic *p = malloc(sizeof *p);
    p->w = w;
    p->h = h;
    p->stride = w * 4;
    p->data = malloc((size_t)p->stride * h);
    return p;
}

void qv_wpic_free(qv_wpic *p) {
    if (!p) return;
    free(p->data);
    free(p);
}

static inline uint8_t clamp8(int v) { return v < 0 ? 0 : v > 255 ? 255 : (uint8_t)v; }

// Bilinear sample of one BGRA pixel at fixed-point (16.16) source coords.
static inline void sample(const uint8_t *src, int sw, int sh, int sstride, int fx, int fy, int *b, int *g, int *r) {
    int x0 = fx >> 16, y0 = fy >> 16;
    int ax = (fx >> 8) & 0xFF, ay = (fy >> 8) & 0xFF;
    int x1 = x0 + 1 < sw ? x0 + 1 : sw - 1, y1 = y0 + 1 < sh ? y0 + 1 : sh - 1;
    const uint8_t *p00 = src + y0 * sstride + x0 * 4, *p01 = src + y0 * sstride + x1 * 4;
    const uint8_t *p10 = src + y1 * sstride + x0 * 4, *p11 = src + y1 * sstride + x1 * 4;
    for (int c = 0; c < 3; c++) {
        int top = p00[c] * (256 - ax) + p01[c] * ax;
        int bot = p10[c] * (256 - ax) + p11[c] * ax;
        int v = (top * (256 - ay) + bot * ay) >> 16;
        if (c == 0) *b = v;
        else if (c == 1) *g = v;
        else *r = v;
    }
}

void qv_bgra_to_nv12(const uint8_t *src, int sw, int sh, int sstride, uint8_t *y, int ystride, uint8_t *uv,
                     int uvstride, int dw, int dh) {
    int stepx = (int)(((int64_t)sw << 16) / dw), stepy = (int)(((int64_t)sh << 16) / dh);
    for (int j = 0; j < dh; j++) {
        int fy = j * stepy + stepy / 2 - 32768;
        if (fy < 0) fy = 0;
        uint8_t *yr = y + j * ystride;
        for (int i = 0; i < dw; i++) {
            int fx = i * stepx + stepx / 2 - 32768;
            if (fx < 0) fx = 0;
            int b, g, r;
            sample(src, sw, sh, sstride, fx, fy, &b, &g, &r);
            yr[i] = clamp8(((66 * r + 129 * g + 25 * b + 128) >> 8) + 16);
            if ((j & 1) == 0 && (i & 1) == 0) {
                uint8_t *uvp = uv + (j / 2) * uvstride + i;
                uvp[0] = clamp8(((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128);
                uvp[1] = clamp8(((112 * r - 94 * g - 18 * b + 128) >> 8) + 128);
            }
        }
    }
}

void qv_nv12_to_bgra(const uint8_t *y, int ystride, const uint8_t *uv, int uvstride, int w, int h, uint8_t *dst,
                     int dstride) {
    for (int j = 0; j < h; j++) {
        const uint8_t *yr = y + j * ystride;
        const uint8_t *uvr = uv + (j / 2) * uvstride;
        uint8_t *o = dst + j * dstride;
        for (int i = 0; i < w; i++) {
            int c = yr[i] - 16, d = uvr[i & ~1] - 128, e = uvr[(i & ~1) + 1] - 128;
            o[i * 4 + 0] = clamp8((298 * c + 516 * d + 128) >> 8);
            o[i * 4 + 1] = clamp8((298 * c - 100 * d - 208 * e + 128) >> 8);
            o[i * 4 + 2] = clamp8((298 * c + 409 * e + 128) >> 8);
            o[i * 4 + 3] = 255;
        }
    }
}
