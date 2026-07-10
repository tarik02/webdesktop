package media

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0

#include <gst/gst.h>
#include <gst/video/video-event.h>

static gboolean webdesktop_request_keyframe(void *target) {
	GstEvent *event = gst_video_event_new_upstream_force_key_unit(
		GST_CLOCK_TIME_NONE,
		TRUE,
		0
	);
	return gst_element_send_event(GST_ELEMENT(target), event);
}
*/
import "C"

import "github.com/go-gst/go-gst/gst"

func requestGStreamerKeyframe(target *gst.Element) bool {
	return C.webdesktop_request_keyframe(target.Unsafe()) == C.TRUE
}
