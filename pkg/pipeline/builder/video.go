// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package builder

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/go-gst/go-gst/gst"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/gstreamer"
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	videoTestSrcName = "video_test_src"
	keyframeDefault  = 1.0
	//UseVAAPI         = true
)

var UseVAAPI = len(os.Getenv("GST_VAAPI_ALL_DRIVERS")) > 0

type VideoBin struct {
	bin  *gstreamer.Bin
	conf *config.PipelineConfig

	mu          sync.Mutex
	nextID      int
	selectedPad string
	lastPTS     uint64
	pads        map[string]*gst.Pad
	names       map[string]string
	selector    *gst.Element
	rawVideoTee *gst.Element
}

func BuildVideoBin(pipeline *gstreamer.Pipeline, p *config.PipelineConfig) error {
	b := &VideoBin{
		bin:  pipeline.NewBin("video"),
		conf: p,
	}

	switch p.SourceType {
	case types.SourceTypeWeb:
		if err := b.buildWebInput(); err != nil {
			return err
		}

	case types.SourceTypeSDK:
		if err := b.buildSDKInput(); err != nil {
			return err
		}

		pipeline.AddOnTrackAdded(b.onTrackAdded)
		pipeline.AddOnTrackRemoved(b.onTrackRemoved)
		pipeline.AddOnTrackMuted(b.onTrackMuted)
		pipeline.AddOnTrackUnmuted(b.onTrackUnmuted)
	}

	var getPad func() *gst.Pad
	if len(p.GetEncodedOutputs()) > 1 {
		tee, err := gst.NewElementWithName("tee", "video_tee")
		if err != nil {
			return errors.ErrGstPipelineError(err)
		}

		if err = b.bin.AddElement(tee); err != nil {
			return err
		}

		getPad = func() *gst.Pad {
			return tee.GetRequestPad("src_%u")
		}
	} else if len(p.GetEncodedOutputs()) > 0 {
		queue, err := gstreamer.BuildQueue("video_queue", config.Latency, true)
		if err != nil {
			return errors.ErrGstPipelineError(err)
		}
		if err = b.bin.AddElement(queue); err != nil {
			return err
		}

		getPad = func() *gst.Pad {
			return queue.GetStaticPad("src")
		}
	}

	b.bin.SetGetSinkPad(func(name string) *gst.Pad {
		if strings.HasPrefix(name, "image") {
			return b.rawVideoTee.GetRequestPad("src_%u")
		} else if getPad != nil {
			return getPad()
		}

		return nil
	})

	return pipeline.AddSourceBin(b.bin)
}

func (b *VideoBin) onTrackAdded(ts *config.TrackSource) {
	if b.bin.GetState() > gstreamer.StateRunning {
		return
	}

	if ts.Kind == lksdk.TrackKindVideo {
		if err := b.addAppSrcBin(ts); err != nil {
			b.bin.OnError(err)
		}
	}
}

func (b *VideoBin) onTrackRemoved(trackID string) {
	if b.bin.GetState() > gstreamer.StateRunning {
		return
	}

	b.mu.Lock()
	name, ok := b.names[trackID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.names, trackID)
	delete(b.pads, name)

	if b.selectedPad == name {
		if err := b.setSelectorPadLocked(videoTestSrcName); err != nil {
			b.mu.Unlock()
			b.bin.OnError(err)
			return
		}
	}
	b.mu.Unlock()

	if err := b.bin.RemoveSourceBin(name); err != nil {
		b.bin.OnError(err)
	}
}

func (b *VideoBin) onTrackMuted(trackID string) {
	if b.bin.GetState() > gstreamer.StateRunning {
		return
	}

	b.mu.Lock()
	if name, ok := b.names[trackID]; ok && b.selectedPad == name {
		if err := b.setSelectorPadLocked(videoTestSrcName); err != nil {
			b.mu.Unlock()
			b.bin.OnError(err)
			return
		}
	}
	b.mu.Unlock()
}

func (b *VideoBin) onTrackUnmuted(trackID string) {
	if b.bin.GetState() > gstreamer.StateRunning {
		return
	}

	b.mu.Lock()
	if name, ok := b.names[trackID]; ok {
		if err := b.setSelectorPadLocked(name); err != nil {
			b.mu.Unlock()
			b.bin.OnError(err)
			return
		}
	}
	b.mu.Unlock()
}

func (b *VideoBin) buildWebInput() error {
	xImageSrc, err := gst.NewElement("ximagesrc")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = xImageSrc.SetProperty("display-name", b.conf.Display); err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = xImageSrc.SetProperty("use-damage", false); err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = xImageSrc.SetProperty("show-pointer", false); err != nil {
		return errors.ErrGstPipelineError(err)
	}

	videoQueue, err := gstreamer.BuildQueue("video_input_queue", config.Latency, true)
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	videoConvert, err := gst.NewElement("videoconvert")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	videoRate, err := gst.NewElement("videorate")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = videoRate.SetProperty("skip-to-first", true); err != nil {
		return errors.ErrGstPipelineError(err)
	}

	caps, err := gst.NewElement("capsfilter")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = caps.SetProperty("caps", gst.NewCapsFromString(fmt.Sprintf(
		"video/x-raw,framerate=%d/1",
		b.conf.Framerate,
	),
	)); err != nil {
		return errors.ErrGstPipelineError(err)
	}

	if err = b.bin.AddElements(xImageSrc, videoQueue, videoConvert, videoRate, caps); err != nil {
		return err
	}

	if err := b.addDecodedVideoSink(); err != nil {
		return err
	}

	return nil
}

func (b *VideoBin) buildSDKInput() error {
	b.pads = make(map[string]*gst.Pad)
	b.names = make(map[string]string)

	// add selector first so pads can be created
	if b.conf.VideoDecoding {
		if err := b.addSelector(); err != nil {
			return err
		}
	}

	if b.conf.VideoTrack != nil {
		if err := b.addAppSrcBin(b.conf.VideoTrack); err != nil {
			return err
		}
	}

	if b.conf.VideoDecoding {
		b.bin.SetGetSrcPad(b.getSrcPad)
		b.bin.SetEOSFunc(func() bool {
			b.mu.Lock()
			selected := b.selectedPad
			pad := b.pads[videoTestSrcName]
			b.mu.Unlock()

			if selected == videoTestSrcName {
				pad.SendEvent(gst.NewEOSEvent())
			}
			return false
		})

		if err := b.addVideoTestSrcBin(); err != nil {
			return err
		}
		if b.conf.VideoTrack == nil {
			if err := b.setSelectorPad(videoTestSrcName); err != nil {
				return err
			}
		}
		if err := b.addDecodedVideoSink(); err != nil {
			return err
		}
	}

	return nil
}

func (b *VideoBin) addAppSrcBin(ts *config.TrackSource) error {
	name := fmt.Sprintf("%s_%d", ts.TrackID, b.nextID)
	b.nextID++

	appSrcBin, err := b.buildAppSrcBin(ts, name)
	if err != nil {
		return err
	}

	if b.conf.VideoDecoding {
		b.createSrcPad(ts.TrackID, name)
	}

	if err = b.bin.AddSourceBin(appSrcBin); err != nil {
		return err
	}

	if b.conf.VideoDecoding {
		return b.setSelectorPad(name)
	}

	return nil
}

func (b *VideoBin) buildAppSrcBin(ts *config.TrackSource, name string) (*gstreamer.Bin, error) {
	appSrcBin := b.bin.NewBin(name)
	ts.AppSrc.Element.SetArg("format", "time")
	if err := ts.AppSrc.Element.SetProperty("is-live", true); err != nil {
		return nil, err
	}
	if err := appSrcBin.AddElement(ts.AppSrc.Element); err != nil {
		return nil, err
	}

	switch ts.MimeType {
	case types.MimeTypeH264:
		if err := ts.AppSrc.Element.SetProperty("caps", gst.NewCapsFromString(fmt.Sprintf(
			"application/x-rtp,media=video,payload=%d,encoding-name=H264,clock-rate=%d",
			ts.PayloadType, ts.ClockRate,
		))); err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}

		rtpH264Depay, err := gst.NewElement("rtph264depay")
		if err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}

		caps, err := gst.NewElement("capsfilter")
		if err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}
		if err = caps.SetProperty("caps", gst.NewCapsFromString(
			"video/x-h264,stream-format=byte-stream",
		)); err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}

		if err = appSrcBin.AddElements(rtpH264Depay, caps); err != nil {
			return nil, err
		}

		if b.conf.VideoDecoding {
			h264Parse, err := gst.NewElement("h264parse")
			if err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}
			if err = appSrcBin.AddElement(h264Parse); err != nil {
				return nil, err
			}

			if UseVAAPI {
				vaapidec, err := gst.NewElement("vaapih264dec")
				if err == nil {
					if err = appSrcBin.AddElement(vaapidec); err == nil {
						break
					}
				}
				logger.Warnw("VAAPI decoder not available, falling back to software", err)
			}

			avdec, err := gst.NewElement("avdec_h264")
			if err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}
			if err = appSrcBin.AddElement(avdec); err != nil {
				return nil, err
			}
		}

	case types.MimeTypeVP8:
		if err := ts.AppSrc.Element.SetProperty("caps", gst.NewCapsFromString(fmt.Sprintf(
			"application/x-rtp,media=video,payload=%d,encoding-name=VP8,clock-rate=%d",
			ts.PayloadType, ts.ClockRate,
		))); err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}

		rtpVP8Depay, err := gst.NewElement("rtpvp8depay")
		if err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}
		if err = appSrcBin.AddElement(rtpVP8Depay); err != nil {
			return nil, err
		}

		if b.conf.VideoDecoding {
			vp8Dec, err := gst.NewElement("vp8dec")
			if err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}
			if err = appSrcBin.AddElement(vp8Dec); err != nil {
				return nil, err
			}
		} else {
			return appSrcBin, nil
		}

	case types.MimeTypeVP9:
		if err := ts.AppSrc.Element.SetProperty("caps", gst.NewCapsFromString(fmt.Sprintf(
			"application/x-rtp,media=video,payload=%d,encoding-name=VP9,clock-rate=%d",
			ts.PayloadType, ts.ClockRate,
		))); err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}

		rtpVP9Depay, err := gst.NewElement("rtpvp9depay")
		if err != nil {
			return nil, errors.ErrGstPipelineError(err)
		}
		if err = appSrcBin.AddElement(rtpVP9Depay); err != nil {
			return nil, err
		}

		if b.conf.VideoDecoding {
			vp9Dec, err := gst.NewElement("vp9dec")
			if err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}
			if err = appSrcBin.AddElement(vp9Dec); err != nil {
				return nil, err
			}
		} else {
			vp9Parse, err := gst.NewElement("vp9parse")
			if err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}

			vp9Caps, err := gst.NewElement("capsfilter")
			if err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}
			if err = vp9Caps.SetProperty("caps", gst.NewCapsFromString(
				"video/x-vp9,width=[16,2147483647],height=[16,2147483647]",
			)); err != nil {
				return nil, errors.ErrGstPipelineError(err)
			}

			if err = appSrcBin.AddElements(vp9Parse, vp9Caps); err != nil {
				return nil, err
			}

			return appSrcBin, nil
		}

	default:
		return nil, errors.ErrNotSupported(string(ts.MimeType))
	}

	if err := b.addVideoConverter(appSrcBin); err != nil {
		return nil, err
	}

	return appSrcBin, nil
}

func (b *VideoBin) addVideoTestSrcBin() error {
	testSrcBin := b.bin.NewBin(videoTestSrcName)
	if err := b.bin.AddSourceBin(testSrcBin); err != nil {
		return err
	}

	// Create test source - use the proper element name
	videoTestSrc, err := gst.NewElement("videotestsrc")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = videoTestSrc.SetProperty("is-live", true); err != nil {
		return errors.ErrGstPipelineError(err)
	}
	videoTestSrc.SetArg("pattern", "black")

	// Build queue with our identifying prefix
	queue, err := gstreamer.BuildQueue(fmt.Sprintf("%s_queue", videoTestSrcName),
		config.Latency, false)
	if err != nil {
		return err
	}
	//if err = queue.SetProperty("min-threshold-time", uint64(2e9)); err != nil {
	if err = queue.SetProperty("min-threshold-time", uint64(100*1000*1000)); err != nil {
		return errors.ErrGstPipelineError(err)
	}

	// Insert videoconvert to ensure format conversion to NV12
	videoconvert, err := gst.NewElement("videoconvert")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	caps, err := b.newVideoCapsFilter(true)
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	// Add elements in the correct order: videotestsrc -> queue -> videoconvert -> capsfilter
	if err = testSrcBin.AddElements(videoTestSrc, queue, videoconvert, caps); err != nil {
		return err
	}

	b.createTestSrcPad()
	return nil
}

func (b *VideoBin) addSelector() error {
	inputSelector, err := gst.NewElement("input-selector")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	videoRate, err := gst.NewElement("videorate")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = videoRate.SetProperty("skip-to-first", true); err != nil {
		return errors.ErrGstPipelineError(err)
	}

	caps, err := b.newVideoCapsFilter(true)
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	if err = b.bin.AddElements(inputSelector, videoRate, caps); err != nil {
		return err
	}

	b.selector = inputSelector
	return nil
}

func (b *VideoBin) addEncoder() error {
	videoQueue, err := gstreamer.BuildQueue("video_encoder_queue", config.Latency, false)
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = b.bin.AddElement(videoQueue); err != nil {
		return err
	}

	switch b.conf.VideoOutCodec {
	case types.MimeTypeH264:
		if UseVAAPI {
			vaapih264enc, err := gst.NewElement("vaapih264enc")
			if err == nil {
				vaapih264enc.SetArg("rate-control", "cbr")
				vaapih264enc.SetArg("tune", "low-latency")
				if err = vaapih264enc.SetProperty("bitrate", uint(b.conf.VideoBitrate)); err == nil {
					// Set keyframe interval: either from config or default to 1 second
					var keyframeInterval uint
					if b.conf.KeyFrameInterval != 0 {
						keyframeInterval = uint(b.conf.KeyFrameInterval * float64(b.conf.Framerate))
					} else {
						keyframeInterval = uint(keyframeDefault * float64(b.conf.Framerate))
					}

					if err = vaapih264enc.SetProperty("keyframe-period", keyframeInterval); err == nil {
						h264parse, err := gst.NewElement("h264parse")
						if err == nil {
							h264parse.SetArg("config-interval", "-1")

							caps, err := gst.NewElement("capsfilter")
							if err == nil {
								caps.SetProperty("caps", gst.NewCapsFromString(fmt.Sprintf(
									"video/x-h264,profile=%s",
									b.conf.VideoProfile,
								)))

								if err = b.bin.AddElements(vaapih264enc, h264parse, caps); err == nil {
									return nil
								}
							}
						}
					}
				}
			}
			logger.Warnw("VAAPI encoder setup failed, falling back to software", err)
		}

		// Software fallback
		x264enc, err := gst.NewElement("x264enc")
		if err != nil {
			return errors.ErrGstPipelineError(err)
		}

		x264enc.SetArg("speed-preset", "veryfast")
		x264enc.SetArg("tune", "zerolatency")
		x264enc.SetProperty("bitrate", uint(b.conf.VideoBitrate))
		x264enc.SetProperty("vbv-buf-capacity", uint(100))

		// Set keyframe interval: either from config or default to 1 second
		var keyframeInterval uint
		if b.conf.KeyFrameInterval != 0 {
			keyframeInterval = uint(b.conf.KeyFrameInterval * float64(b.conf.Framerate))
		} else {
			keyframeInterval = uint(keyframeDefault * float64(b.conf.Framerate))
		}
		x264enc.SetProperty("key-int-max", keyframeInterval)

		var options []string
		if sc := b.conf.GetStreamConfig(); sc != nil && sc.OutputType == types.OutputTypeRTMP {
			options = append(options, "nal-hrd=cbr")
		}
		if len(options) > 0 {
			x264enc.SetProperty("option-string", strings.Join(options, ":"))
		}

		caps, err := gst.NewElement("capsfilter")
		if err != nil {
			return errors.ErrGstPipelineError(err)
		}
		caps.SetProperty("caps", gst.NewCapsFromString(fmt.Sprintf(
			"video/x-h264,profile=%s",
			b.conf.VideoProfile,
		)))

		return b.bin.AddElements(x264enc, caps)
	}

	return errors.ErrNotSupported(fmt.Sprintf("%s encoding", b.conf.VideoOutCodec))
}

func (b *VideoBin) addDecodedVideoSink() error {
	var err error
	b.rawVideoTee, err = gst.NewElement("tee")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	if err = b.bin.AddElement(b.rawVideoTee); err != nil {
		return err
	}

	if b.conf.VideoEncoding {
		err = b.addEncoder()
		if err != nil {
			return err
		}
	}

	return nil
}

func (b *VideoBin) addVideoConverter(bin *gstreamer.Bin) error {
	videoQueue, err := gstreamer.BuildQueue("video_input_queue", config.Latency, true)
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	videoConvert, err := gst.NewElement("videoconvert")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	videoScale, err := gst.NewElement("videoscale")
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}

	elements := []*gst.Element{videoQueue, videoConvert, videoScale}

	if !b.conf.VideoDecoding {
		videoRate, err := gst.NewElement("videorate")
		if err != nil {
			return errors.ErrGstPipelineError(err)
		}
		if err = videoRate.SetProperty("skip-to-first", true); err != nil {
			return errors.ErrGstPipelineError(err)
		}
		elements = append(elements, videoRate)
	}

	caps, err := b.newVideoCapsFilter(!b.conf.VideoDecoding)
	if err != nil {
		return errors.ErrGstPipelineError(err)
	}
	elements = append(elements, caps)

	return bin.AddElements(elements...)
}

func (b *VideoBin) newVideoCapsFilter(includeFramerate bool) (*gst.Element, error) {
	caps, err := gst.NewElement("capsfilter")
	if err != nil {
		return nil, errors.ErrGstPipelineError(err)
	}
	var capsStr string
	if includeFramerate {
		capsStr = fmt.Sprintf(
			"video/x-raw,framerate=%d/1,format=NV12,width=%d,height=%d,colorimetry=bt709,chroma-site=mpeg2,pixel-aspect-ratio=1/1",
			b.conf.Framerate, b.conf.Width, b.conf.Height,
		)
	} else {
		capsStr = fmt.Sprintf(
			"video/x-raw,format=NV12,width=%d,height=%d,colorimetry=bt709,chroma-site=mpeg2,pixel-aspect-ratio=1/1",
			b.conf.Width, b.conf.Height,
		)
	}
	if err = caps.SetProperty("caps", gst.NewCapsFromString(capsStr)); err != nil {
		return nil, errors.ErrGstPipelineError(err)
	}
	return caps, nil
}

func (b *VideoBin) getSrcPad(name string) *gst.Pad {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.pads[name]
}

func (b *VideoBin) createSrcPad(trackID, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.names[trackID] = name

	pad := b.selector.GetRequestPad("sink_%u")
	pad.AddProbe(gst.PadProbeTypeBuffer, func(pad *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		pts := uint64(info.GetBuffer().PresentationTimestamp())
		b.mu.Lock()
		if pts < b.lastPTS || (b.selectedPad != videoTestSrcName && b.selectedPad != name) {
			b.mu.Unlock()
			return gst.PadProbeDrop
		}
		b.lastPTS = pts
		b.mu.Unlock()
		return gst.PadProbeOK
	})

	b.pads[name] = pad
}

func (b *VideoBin) createTestSrcPad() {
	b.mu.Lock()
	defer b.mu.Unlock()

	pad := b.selector.GetRequestPad("sink_%u")
	pad.AddProbe(gst.PadProbeTypeBuffer, func(pad *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		pts := uint64(info.GetBuffer().PresentationTimestamp())
		b.mu.Lock()
		if pts < b.lastPTS || (b.selectedPad != videoTestSrcName) {
			b.mu.Unlock()
			return gst.PadProbeDrop
		}
		b.lastPTS = pts
		b.mu.Unlock()
		return gst.PadProbeOK
	})

	b.pads[videoTestSrcName] = pad
}

func (b *VideoBin) setSelectorPad(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.setSelectorPadLocked(name)
}

func (b *VideoBin) setSelectorPadLocked(name string) error {
	pad := b.pads[name]

	// drop until the next keyframe
	pad.AddProbe(gst.PadProbeTypeBuffer, func(pad *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		buffer := info.GetBuffer()
		if buffer.HasFlags(gst.BufferFlagDeltaUnit) {
			return gst.PadProbeDrop
		}
		logger.Debugw("active pad changed", "name", name)
		return gst.PadProbeRemove
	})

	if err := b.selector.SetProperty("active-pad", pad); err != nil {
		return errors.ErrGstPipelineError(err)
	}

	b.selectedPad = name
	return nil
}
