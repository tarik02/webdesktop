package media

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0

#include <gst/gst.h>
#include <gst/video/video.h>

static GstSample *webdesktop_video_sample_normalize(void *sample_ptr) {
	GstSample *sample = GST_SAMPLE(sample_ptr);
	GstBuffer *source_buffer = gst_sample_get_buffer(sample);
	GstCaps *caps = gst_sample_get_caps(sample);
	GstVideoMeta *meta;
	GstBuffer *destination_buffer = NULL;
	GstSample *result = NULL;
	GstVideoInfo info;
	GstVideoFrame source_frame = GST_VIDEO_FRAME_INIT;
	GstVideoFrame destination_frame = GST_VIDEO_FRAME_INIT;
	gboolean source_mapped = FALSE;
	gboolean destination_mapped = FALSE;

	if (source_buffer == NULL || caps == NULL || !gst_video_info_from_caps(&info, caps))
		return NULL;

	meta = gst_buffer_get_video_meta(source_buffer);
	if (meta == NULL) {
		if (gst_buffer_get_size(source_buffer) < info.size)
			return NULL;
		return gst_sample_ref(sample);
	}
	if (info.width != meta->width || info.height != meta->height ||
	    GST_VIDEO_INFO_N_PLANES(&info) != meta->n_planes)
		return NULL;
	if (!gst_video_frame_map(&source_frame, &info, source_buffer, GST_MAP_READ))
		return NULL;
	source_mapped = TRUE;

	destination_buffer = gst_buffer_new_allocate(NULL, info.size, NULL);
	if (destination_buffer == NULL)
		goto done;
	if (gst_buffer_add_video_meta_full(
		destination_buffer,
		source_frame.flags,
		GST_VIDEO_INFO_FORMAT(&info),
		info.width,
		info.height,
		GST_VIDEO_INFO_N_PLANES(&info),
		info.offset,
		info.stride
	) == NULL)
		goto done;
	if (!gst_video_frame_map(&destination_frame, &info, destination_buffer, GST_MAP_WRITE))
		goto done;
	destination_mapped = TRUE;
	if (!gst_video_frame_copy(&destination_frame, &source_frame))
		goto done;

	gst_video_frame_unmap(&destination_frame);
	destination_mapped = FALSE;
	gst_video_frame_unmap(&source_frame);
	source_mapped = FALSE;
	if (!gst_buffer_copy_into(
		destination_buffer,
		source_buffer,
		GST_BUFFER_COPY_FLAGS | GST_BUFFER_COPY_TIMESTAMPS,
		0,
		-1
	))
		goto done;

	result = gst_sample_copy(sample);
	if (result != NULL)
		gst_sample_set_buffer(result, destination_buffer);

done:
	if (destination_mapped)
		gst_video_frame_unmap(&destination_frame);
	if (source_mapped)
		gst_video_frame_unmap(&source_frame);
	if (destination_buffer != NULL)
		gst_buffer_unref(destination_buffer);
	return result;
}
*/
import "C"

import (
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

func normalizeVideoSampleLayout(sample *gst.Sample) *gst.Sample {
	normalized := C.webdesktop_video_sample_normalize(unsafe.Pointer(sample.Instance()))
	if normalized == nil {
		return nil
	}
	return gst.FromGstSampleUnsafeFull(unsafe.Pointer(normalized))
}
