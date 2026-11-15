// VideoToolbox H.264 encode/decode for package media.
#import <Foundation/Foundation.h>
#import <VideoToolbox/VideoToolbox.h>
#import <CoreMedia/CoreMedia.h>
#include <stdlib.h>
#include <string.h>
#include "media_darwin.h"

// Implemented in Go (cgo exports).
extern void qvGoEncOutput(uintptr_t handle, void *avcc, int len, int len_size, void *sps, int sps_len,
                          void *pps, int pps_len, int key, int64_t us);
extern void qvGoEncError(uintptr_t handle, int status);
extern void qvGoDecOutput(uintptr_t handle, void *pb, int64_t us);

static char *errorf(NSString *fmt, OSStatus st) {
    return strdup([[NSString stringWithFormat:@"%@ (OSStatus %d)", fmt, (int)st] UTF8String]);
}

// ---------------------------------------------------------------- encoder

struct qv_enc {
    VTCompressionSessionRef session;
    VTPixelTransferSessionRef transfer;
    int width, height;
    uintptr_t handle;
};

static void encOutput(void *refcon, void *frameRefcon, OSStatus status, VTEncodeInfoFlags flags,
                      CMSampleBufferRef sb) {
    struct qv_enc *e = (struct qv_enc *)refcon;
    if (status != noErr) {
        qvGoEncError(e->handle, (int)status);
        return;
    }
    if (sb == NULL || (flags & kVTEncodeInfo_FrameDropped)) return;
    CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
    int key = 1;
    if (atts && CFArrayGetCount(atts) > 0) {
        CFDictionaryRef a = CFArrayGetValueAtIndex(atts, 0);
        CFBooleanRef notSync = CFDictionaryGetValue(a, kCMSampleAttachmentKey_NotSync);
        key = !(notSync && CFBooleanGetValue(notSync));
    }
    const uint8_t *sps = NULL, *pps = NULL;
    size_t spsLen = 0, ppsLen = 0, count = 0;
    int lenSize = 4;
    CMFormatDescriptionRef fd = CMSampleBufferGetFormatDescription(sb);
    if (fd) {
        CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, 0, &sps, &spsLen, &count, &lenSize);
        CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, 1, &pps, &ppsLen, &count, NULL);
    }
    CMBlockBufferRef bb = CMSampleBufferGetDataBuffer(sb);
    size_t len = CMBlockBufferGetDataLength(bb);
    void *buf = malloc(len);
    if (CMBlockBufferCopyDataBytes(bb, 0, len, buf) != kCMBlockBufferNoErr) {
        free(buf);
        return;
    }
    CMTime pts = CMSampleBufferGetPresentationTimeStamp(sb);
    int64_t us = CMTIME_IS_VALID(pts) ? (int64_t)CMTimeConvertScale(pts, 1000000, kCMTimeRoundingMethod_Default).value : 0;
    qvGoEncOutput(e->handle, buf, (int)len, lenSize, (void *)sps, (int)spsLen, (void *)pps, (int)ppsLen, key, us);
    free(buf);
}

static void setProp(VTCompressionSessionRef s, CFStringRef key, CFTypeRef value) {
    VTSessionSetProperty(s, key, value); // best effort: unsupported keys are ignored
}

qv_enc *qv_enc_new(int width, int height, int fps, int bitrate, int gop_frames, int screen,
                   uintptr_t handle, char **err, int *hardware) {
    struct qv_enc *e = calloc(1, sizeof(*e));
    e->width = width;
    e->height = height;
    e->handle = handle;

    // Hardware encoder with low-latency rate control (no frame reordering,
    // short lookahead); fall back to plain hardware, then anything.
    NSArray *specs = @[
        @{(__bridge NSString *)kVTVideoEncoderSpecification_EnableLowLatencyRateControl : @YES,
          (__bridge NSString *)kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder : @YES},
        @{(__bridge NSString *)kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder : @YES},
        @{},
    ];
    OSStatus st = -1;
    for (NSUInteger i = 0; i < specs.count; i++) {
        st = VTCompressionSessionCreate(NULL, width, height, kCMVideoCodecType_H264,
                                        (__bridge CFDictionaryRef)specs[i], NULL, NULL, encOutput, e,
                                        &e->session);
        if (st == noErr) {
            *hardware = i < 2;
            break;
        }
    }
    if (st != noErr) {
        *err = errorf(@"VTCompressionSessionCreate failed", st);
        free(e);
        return NULL;
    }
    VTCompressionSessionRef s = e->session;
    setProp(s, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
    setProp(s, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse); // no B-frames
    setProp(s, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_ConstrainedHigh_AutoLevel);
    setProp(s, kVTCompressionPropertyKey_AverageBitRate, (__bridge CFTypeRef) @(bitrate));
    // Cap bursts at 1.5× the average over a second (keyframes still fit).
    NSArray *limits = @[ @(bitrate * 3 / 16), @1 ];
    setProp(s, kVTCompressionPropertyKey_DataRateLimits, (__bridge CFTypeRef)limits);
    setProp(s, kVTCompressionPropertyKey_ExpectedFrameRate, (__bridge CFTypeRef) @(fps));
    setProp(s, kVTCompressionPropertyKey_MaxKeyFrameInterval, (__bridge CFTypeRef) @(gop_frames));
    setProp(s, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration,
            (__bridge CFTypeRef) @((double)gop_frames / (double)fps));
    setProp(s, kVTCompressionPropertyKey_MaxFrameDelayCount, (__bridge CFTypeRef) @0);
    if (screen) {
        // Text-heavy and mostly static: favour sharpness over motion.
        setProp(s, kVTCompressionPropertyKey_Quality, (__bridge CFTypeRef) @0.75);
    }
    st = VTCompressionSessionPrepareToEncodeFrames(s);
    if (st != noErr) {
        *err = errorf(@"VTCompressionSessionPrepareToEncodeFrames failed", st);
        qv_enc_close(e);
        return NULL;
    }
    st = VTPixelTransferSessionCreate(NULL, &e->transfer);
    if (st != noErr) {
        *err = errorf(@"VTPixelTransferSessionCreate failed", st);
        qv_enc_close(e);
        return NULL;
    }
    VTSessionSetProperty(e->transfer, kVTPixelTransferPropertyKey_ScalingMode, kVTScalingMode_Letterbox);
    return e;
}

int qv_enc_encode(qv_enc *e, void *pbv, int64_t us, int force) {
    CVPixelBufferRef pb = (CVPixelBufferRef)pbv;
    CVPixelBufferRef in = pb;
    CVPixelBufferRef scaled = NULL;
    if ((int)CVPixelBufferGetWidth(pb) != e->width || (int)CVPixelBufferGetHeight(pb) != e->height) {
        CVPixelBufferPoolRef pool = VTCompressionSessionGetPixelBufferPool(e->session);
        if (pool == NULL || CVPixelBufferPoolCreatePixelBuffer(NULL, pool, &scaled) != kCVReturnSuccess) return -1;
        OSStatus st = VTPixelTransferSessionTransferImage(e->transfer, pb, scaled);
        if (st != noErr) {
            CVPixelBufferRelease(scaled);
            return (int)st;
        }
        in = scaled;
    }
    NSDictionary *props = force ? @{(__bridge NSString *)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES} : nil;
    OSStatus st = VTCompressionSessionEncodeFrame(e->session, in, CMTimeMake(us, 1000000), kCMTimeInvalid,
                                                  (__bridge CFDictionaryRef)props, NULL, NULL);
    if (scaled) CVPixelBufferRelease(scaled);
    return (int)st;
}

void qv_enc_close(qv_enc *e) {
    if (e == NULL) return;
    if (e->session) {
        VTCompressionSessionCompleteFrames(e->session, kCMTimeInvalid);
        VTCompressionSessionInvalidate(e->session);
        CFRelease(e->session);
    }
    if (e->transfer) {
        VTPixelTransferSessionInvalidate(e->transfer);
        CFRelease(e->transfer);
    }
    free(e);
}

// ---------------------------------------------------------------- decoder

struct qv_dec {
    VTDecompressionSessionRef session;
    CMVideoFormatDescriptionRef format;
    uintptr_t handle;
};

static void decOutput(void *refcon, void *frameRefcon, OSStatus status, VTDecodeInfoFlags flags,
                      CVImageBufferRef img, CMTime pts, CMTime dur) {
    struct qv_dec *d = (struct qv_dec *)refcon;
    if (status != noErr || img == NULL) return;
    qvGoDecOutput(d->handle, (void *)img, (int64_t)CMTimeConvertScale(pts, 1000000, kCMTimeRoundingMethod_Default).value);
}

qv_dec *qv_dec_new(uintptr_t handle) {
    struct qv_dec *d = calloc(1, sizeof(*d));
    d->handle = handle;
    return d;
}

static void decReset(struct qv_dec *d) {
    if (d->session) {
        VTDecompressionSessionWaitForAsynchronousFrames(d->session);
        VTDecompressionSessionInvalidate(d->session);
        CFRelease(d->session);
        d->session = NULL;
    }
    if (d->format) {
        CFRelease(d->format);
        d->format = NULL;
    }
}

int qv_dec_decode(qv_dec *d, const void *sps, int sps_len, const void *pps, int pps_len, const void *avcc,
                  int len, int64_t us) {
    if (sps && pps) {
        const uint8_t *sets[2] = {sps, pps};
        size_t sizes[2] = {(size_t)sps_len, (size_t)pps_len};
        CMVideoFormatDescriptionRef fd = NULL;
        OSStatus st = CMVideoFormatDescriptionCreateFromH264ParameterSets(NULL, 2, sets, sizes, 4, &fd);
        if (st != noErr) return (int)st;
        if (d->format && CMFormatDescriptionEqual(d->format, fd)) {
            CFRelease(fd);
        } else {
            // New stream parameters (e.g. a layer switch changed the size).
            decReset(d);
            d->format = fd;
            NSDictionary *attrs = @{
                (__bridge NSString *)kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_32BGRA),
                (__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{},
                (__bridge NSString *)kCVPixelBufferMetalCompatibilityKey : @YES,
            };
            VTDecompressionOutputCallbackRecord cb = {decOutput, d};
            st = VTDecompressionSessionCreate(NULL, d->format, NULL, (__bridge CFDictionaryRef)attrs, &cb,
                                              &d->session);
            if (st != noErr) {
                decReset(d);
                return (int)st;
            }
            VTSessionSetProperty(d->session, kVTDecompressionPropertyKey_RealTime, kCFBooleanTrue);
        }
    }
    if (d->session == NULL) return -12909; // kVTVideoDecoderBadDataErr: need a keyframe first
    CMBlockBufferRef bb = NULL;
    OSStatus st = CMBlockBufferCreateWithMemoryBlock(NULL, NULL, len, NULL, NULL, 0, len, 0, &bb);
    if (st != kCMBlockBufferNoErr) return (int)st;
    CMBlockBufferReplaceDataBytes(avcc, bb, 0, len);
    CMSampleBufferRef sb = NULL;
    size_t size = len;
    CMSampleTimingInfo timing = {kCMTimeInvalid, CMTimeMake(us, 1000000), kCMTimeInvalid};
    st = CMSampleBufferCreateReady(NULL, bb, d->format, 1, 1, &timing, 1, &size, &sb);
    CFRelease(bb);
    if (st != noErr) return (int)st;
    st = VTDecompressionSessionDecodeFrame(d->session, sb, 0, NULL, NULL); // synchronous: output before return
    CFRelease(sb);
    return (int)st;
}

void qv_dec_close(qv_dec *d) {
    if (d == NULL) return;
    decReset(d);
    free(d);
}

// ---------------------------------------------------------------- buffers

void *qv_pb_new_bgra(int width, int height) {
    NSDictionary *attrs = @{
        (__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{},
        (__bridge NSString *)kCVPixelBufferMetalCompatibilityKey : @YES,
    };
    CVPixelBufferRef pb = NULL;
    if (CVPixelBufferCreate(NULL, width, height, kCVPixelFormatType_32BGRA, (__bridge CFDictionaryRef)attrs, &pb) !=
        kCVReturnSuccess)
        return NULL;
    return pb;
}

int qv_pb_format(void *pb) { return (int)CVPixelBufferGetPixelFormatType((CVPixelBufferRef)pb); }
int qv_pb_width(void *pb) { return (int)CVPixelBufferGetWidth((CVPixelBufferRef)pb); }
int qv_pb_height(void *pb) { return (int)CVPixelBufferGetHeight((CVPixelBufferRef)pb); }
void qv_pb_retain(void *pb) { CVPixelBufferRetain((CVPixelBufferRef)pb); }
void qv_pb_release(void *pb) { CVPixelBufferRelease((CVPixelBufferRef)pb); }
void *qv_pb_iosurface(void *pb) { return CVPixelBufferGetIOSurface((CVPixelBufferRef)pb); }
void *qv_pb_lock(void *pb, int readonly, int *stride) {
    CVPixelBufferLockBaseAddress((CVPixelBufferRef)pb, readonly ? kCVPixelBufferLock_ReadOnly : 0);
    *stride = (int)CVPixelBufferGetBytesPerRow((CVPixelBufferRef)pb);
    return CVPixelBufferGetBaseAddress((CVPixelBufferRef)pb);
}
void qv_pb_unlock(void *pb, int readonly) {
    CVPixelBufferUnlockBaseAddress((CVPixelBufferRef)pb, readonly ? kCVPixelBufferLock_ReadOnly : 0);
}
