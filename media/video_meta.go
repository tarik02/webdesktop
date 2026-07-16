package media

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0

#include <gst/gst.h>
#include <gst/video/video.h>

static gboolean webdesktop_video_sample_matches_caps(void *sample_ptr) {
	GstSample *sample = GST_SAMPLE(sample_ptr);
	GstBuffer *buffer = gst_sample_get_buffer(sample);
	GstCaps *caps = gst_sample_get_caps(sample);
	GstVideoMeta *meta;
	GstVideoInfo info;

	if (buffer == NULL || caps == NULL || !gst_video_info_from_caps(&info, caps))
		return TRUE;

	meta = gst_buffer_get_video_meta(buffer);
	if (meta == NULL)
		return gst_buffer_get_size(buffer) >= info.size;

	return info.width == meta->width && info.height == meta->height;
}
*/
import "C"

import (
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

func videoSampleMatchesCaps(sample *gst.Sample) bool {
	return C.webdesktop_video_sample_matches_caps(unsafe.Pointer(sample.Instance())) == C.TRUE
}
