#import <Foundation/Foundation.h>
#import <VideoToolbox/VideoToolbox.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#include "encoder_bridge.h"
#include <string.h>

// EncoderContext holds the callback and user data for forwarding encoded NAL units, and the
// buffer used to convert them. Every encoder has its own: with several phones connected,
// each VideoToolbox session calls back on its own thread, and a buffer shared between them
// let one phone's frame overwrite (or be freed under) another's, so phones received each
// other's video.
typedef struct {
    EncodedCallback callback;
    void *userData;
    // Pre-allocated buffer for AVCC→Annex-B conversion to avoid per-NAL malloc/free.
    // Only touched from this encoder's VideoToolbox callback thread.
    uint8_t *annexBuf;
    size_t annexBufCap;
} EncoderContext;

// Annex-B start code: 00 00 00 01
static const uint8_t kStartCode[4] = {0x00, 0x00, 0x00, 0x01};

static uint8_t *EnsureAnnexBuffer(EncoderContext *ctx, size_t needed) {
    if (ctx->annexBufCap < needed) {
        free(ctx->annexBuf);
        ctx->annexBufCap = needed * 2; // double to reduce future reallocs
        ctx->annexBuf = (uint8_t *)malloc(ctx->annexBufCap);
    }
    return ctx->annexBuf;
}

// outputCallback is invoked by VideoToolbox when a frame has been encoded.
// It converts the encoded data from AVCC format (length-prefixed NAL units)
// to Annex-B format (start-code-prefixed) as expected by Android MediaCodec.
static void outputCallback(void *outputCallbackRefCon,
                           void *sourceFrameRefCon,
                           OSStatus status,
                           VTEncodeInfoFlags infoFlags,
                           CMSampleBufferRef sampleBuffer) {
    if (status != noErr || sampleBuffer == NULL) {
        return;
    }

    EncoderContext *ctx = (EncoderContext *)outputCallbackRefCon;
    if (!ctx || !ctx->callback) {
        return;
    }

    // Get the presentation timestamp in microseconds
    CMTime pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer);
    int64_t timestamp = 0;
    if (CMTIME_IS_VALID(pts)) {
        timestamp = (int64_t)(CMTimeGetSeconds(pts) * 1000000.0);
    }

    // Check if this is a keyframe
    CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, false);
    BOOL isKeyframe = NO;
    if (attachments && CFArrayGetCount(attachments) > 0) {
        CFDictionaryRef dict = CFArrayGetValueAtIndex(attachments, 0);
        CFBooleanRef notSync = CFDictionaryGetValue(dict, kCMSampleAttachmentKey_NotSync);
        isKeyframe = (notSync == NULL || !CFBooleanGetValue(notSync));
    }

    // For keyframes, always send SPS/PPS before the IDR slice.
    // The client decoder may restart at any time (e.g. screen lock/unlock),
    // so every keyframe must be self-contained for decoder recovery.
    if (isKeyframe) {
        CMFormatDescriptionRef formatDesc = CMSampleBufferGetFormatDescription(sampleBuffer);
        if (formatDesc) {
            size_t paramCount = 0;
            CMVideoFormatDescriptionGetH264ParameterSetAtIndex(formatDesc, 0, NULL, NULL, &paramCount, NULL);

            for (size_t i = 0; i < paramCount; i++) {
                const uint8_t *paramSet = NULL;
                size_t paramSetSize = 0;
                OSStatus paramStatus = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                    formatDesc, i, &paramSet, &paramSetSize, NULL, NULL);

                if (paramStatus != noErr || !paramSet || paramSetSize == 0) continue;

                // Send Annex-B formatted parameter set
                size_t totalSize = 4 + paramSetSize;
                uint8_t *buf = EnsureAnnexBuffer(ctx, totalSize);
                if (buf) {
                    memcpy(buf, kStartCode, 4);
                    memcpy(buf + 4, paramSet, paramSetSize);
                    ctx->callback(buf, (int)totalSize, 1, timestamp, ctx->userData);
                }
            }
        }
    }

    // Get the encoded data block
    CMBlockBufferRef blockBuffer = CMSampleBufferGetDataBuffer(sampleBuffer);
    if (!blockBuffer) {
        return;
    }

    size_t totalLength = 0;
    char *dataPointer = NULL;
    OSStatus blockStatus = CMBlockBufferGetDataPointer(blockBuffer, 0, NULL, &totalLength, &dataPointer);
    if (blockStatus != noErr || !dataPointer) {
        return;
    }

    // Convert from AVCC format (length-prefixed) to Annex-B format (start-code-prefixed).
    // AVCC format: [4-byte big-endian length][NAL unit][4-byte length][NAL unit]...
    size_t offset = 0;
    while (offset < totalLength) {
        // Read the 4-byte big-endian NAL unit length
        if (offset + 4 > totalLength) {
            break;
        }
        uint32_t nalLength = 0;
        memcpy(&nalLength, dataPointer + offset, 4);
        nalLength = CFSwapInt32BigToHost(nalLength);
        offset += 4;

        if (nalLength == 0 || offset + nalLength > totalLength) {
            break;
        }

        // Build Annex-B formatted NAL unit: start code + data (reuse pre-allocated buffer)
        size_t annexBSize = 4 + nalLength;
        uint8_t *buf = EnsureAnnexBuffer(ctx, annexBSize);
        if (buf) {
            memcpy(buf, kStartCode, 4);
            memcpy(buf + 4, dataPointer + offset, nalLength);
            ctx->callback(buf, (int)annexBSize, isKeyframe ? 1 : 0, timestamp, ctx->userData);
        }

        offset += nalLength;
    }
}

EncoderResult CreateEncoder(int width, int height, int fps, int bitrate, EncodedCallback callback, void *userData) {
    EncoderResult result;
    memset(&result, 0, sizeof(result));

    // Allocate the encoder context to hold callback info
    EncoderContext *ctx = (EncoderContext *)calloc(1, sizeof(EncoderContext));
    if (!ctx) {
        snprintf(result.errorMsg, sizeof(result.errorMsg), "failed to allocate encoder context");
        return result;
    }
    ctx->callback = callback;
    ctx->userData = userData;

    // Create the compression session
    VTCompressionSessionRef session = NULL;
    OSStatus status = VTCompressionSessionCreate(
        kCFAllocatorDefault,
        width,
        height,
        kCMVideoCodecType_H264,
        NULL,   // encoderSpecification
        NULL,   // sourceImageBufferAttributes
        NULL,   // compressedDataAllocator
        outputCallback,
        ctx,
        &session
    );

    if (status != noErr) {
        free(ctx);
        snprintf(result.errorMsg, sizeof(result.errorMsg), "VTCompressionSessionCreate failed: %d", (int)status);
        return result;
    }

    // Configure session properties for ultra-low-latency real-time encoding
    VTSessionSetProperty(session, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
    // Baseline profile: uses CAVLC (faster encode/decode than CABAC in Main profile)
    VTSessionSetProperty(session, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_Baseline_AutoLevel);
    VTSessionSetProperty(session, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
    // Force immediate frame output — no internal buffering
    int maxDelay = 0;
    CFNumberRef maxDelayRef = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &maxDelay);
    if (maxDelayRef) {
        VTSessionSetProperty(session, kVTCompressionPropertyKey_MaxFrameDelayCount, maxDelayRef);
        CFRelease(maxDelayRef);
    }

    // Set average bitrate
    CFNumberRef bitrateRef = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &bitrate);
    if (bitrateRef) {
        VTSessionSetProperty(session, kVTCompressionPropertyKey_AverageBitRate, bitrateRef);
        CFRelease(bitrateRef);
    }

    // Set max keyframe interval — every 0.5s for faster error recovery.
    // If a keyframe is lost/corrupted, the decoder recovers in 500ms instead of 1000ms.
    int keyframeInterval = fps / 2;
    if (keyframeInterval < 1) keyframeInterval = 1;
    CFNumberRef keyframeRef = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &keyframeInterval);
    if (keyframeRef) {
        VTSessionSetProperty(session, kVTCompressionPropertyKey_MaxKeyFrameInterval, keyframeRef);
        CFRelease(keyframeRef);
    }

    // Set data rate limits — cap peak bitrate to 1.5x average over 1-second window.
    // This prevents bursty keyframes from saturating the TCP pipe.
    int peakBitrate = bitrate * 3 / 2;
    int limitWindow = 1; // seconds
    int limits[2] = { peakBitrate / 8, limitWindow }; // bytes per second, window
    CFNumberRef limitsArr[2];
    limitsArr[0] = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &limits[0]);
    limitsArr[1] = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &limits[1]);
    if (limitsArr[0] && limitsArr[1]) {
        CFArrayRef limitsArray = CFArrayCreate(kCFAllocatorDefault, (const void **)limitsArr, 2, &kCFTypeArrayCallBacks);
        if (limitsArray) {
            VTSessionSetProperty(session, kVTCompressionPropertyKey_DataRateLimits, limitsArray);
            CFRelease(limitsArray);
        }
    }
    if (limitsArr[0]) CFRelease(limitsArr[0]);
    if (limitsArr[1]) CFRelease(limitsArr[1]);

    // Set expected frame rate
    CFNumberRef fpsRef = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &fps);
    if (fpsRef) {
        VTSessionSetProperty(session, kVTCompressionPropertyKey_ExpectedFrameRate, fpsRef);
        CFRelease(fpsRef);
    }

    // Prepare the session for encoding
    status = VTCompressionSessionPrepareToEncodeFrames(session);
    if (status != noErr) {
        VTCompressionSessionInvalidate(session);
        CFRelease(session);
        free(ctx);
        snprintf(result.errorMsg, sizeof(result.errorMsg), "PrepareToEncodeFrames failed: %d", (int)status);
        return result;
    }

    result.session = session;
    result.context = ctx;
    result.success = 1;
    return result;
}

int EncodeFrame(void *session, CVPixelBufferRef pixelBuffer, int64_t timestamp) {
    if (!session || !pixelBuffer) {
        return -1;
    }

    // Convert microsecond timestamp to CMTime
    CMTime presentationTime = CMTimeMake(timestamp, 1000000);

    OSStatus status = VTCompressionSessionEncodeFrame(
        (VTCompressionSessionRef)session,
        pixelBuffer,
        presentationTime,
        kCMTimeInvalid,  // duration
        NULL,            // frameProperties
        NULL,            // sourceFrameRefcon
        NULL             // infoFlagsOut
    );

    return (int)status;
}

void DestroyEncoder(void *session, void *context) {
    if (session) {
        // Flush any remaining frames
        VTCompressionSessionCompleteFrames((VTCompressionSessionRef)session, kCMTimeInvalid);
        VTCompressionSessionInvalidate((VTCompressionSessionRef)session);
        CFRelease((VTCompressionSessionRef)session);
    }
    if (context) {
        EncoderContext *ctx = (EncoderContext *)context;
        free(ctx->annexBuf);
        free(ctx);
    }
}
