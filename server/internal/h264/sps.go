// Package h264 holds small bitstream helpers for the video we send to the phones.
package h264

import (
	"bytes"
	"errors"
)

// LimitDecoderBuffering rewrites an SPS NAL (Annex B, with its start code) so the stream
// tells decoders to output every frame as soon as it is decoded.
//
// VideoToolbox emits an SPS with no VUI. Without bitstream_restriction a decoder must
// assume it may have to hold as many frames as the level allows (10 at 896x540 level 3.2)
// and some of them, such as Qualcomm's OMX decoder on Android 5, only output once that
// buffer is full. On a desktop that changes a few times per second that means the picture
// arrives many seconds late. Adding a VUI with max_num_reorder_frames=0 and
// max_dec_frame_buffering=max_num_ref_frames makes them output immediately.
//
// The encoder never reorders frames (AllowFrameReordering is off), so the claim is true.
// Anything that is not a plain SPS this function understands, or that already carries a
// VUI, is returned unchanged.
func LimitDecoderBuffering(nal []byte) []byte {
	prefix, ok := startCodeLen(nal)
	if !ok || len(nal) <= prefix || nal[prefix]&0x1f != 7 {
		return nal
	}
	rbsp := unescape(nal[prefix+1:])

	info, vuiFlagBit, err := parseSPS(rbsp)
	if err != nil || info.HasVUI {
		return nal
	}

	w := &bitWriter{}
	w.copyBits(rbsp, vuiFlagBit)
	w.flag(true) // vui_parameters_present_flag
	writeBitstreamRestrictionVUI(w, info.MaxNumRefFrames)
	w.flag(true) // rbsp_stop_one_bit
	w.alignWithZeros()

	out := append([]byte{}, nal[:prefix+1]...)
	return append(out, escape(w.bytes())...)
}

// writeBitstreamRestrictionVUI writes a VUI whose only content is bitstream_restriction.
func writeBitstreamRestrictionVUI(w *bitWriter, maxNumRefFrames uint32) {
	w.flag(false) // aspect_ratio_info_present_flag
	w.flag(false) // overscan_info_present_flag
	w.flag(false) // video_signal_type_present_flag
	w.flag(false) // chroma_loc_info_present_flag
	w.flag(false) // timing_info_present_flag
	w.flag(false) // nal_hrd_parameters_present_flag
	w.flag(false) // vcl_hrd_parameters_present_flag
	w.flag(false) // pic_struct_present_flag
	w.flag(true)  // bitstream_restriction_flag
	w.flag(true)  // motion_vectors_over_pic_boundaries_flag
	w.ue(0)       // max_bytes_per_pic_denom (0 = no limit)
	w.ue(0)       // max_bits_per_mb_denom (0 = no limit)
	w.ue(16)      // log2_max_mv_length_horizontal
	w.ue(16)      // log2_max_mv_length_vertical
	w.ue(0)       // max_num_reorder_frames
	if maxNumRefFrames < 1 {
		maxNumRefFrames = 1
	}
	w.ue(maxNumRefFrames) // max_dec_frame_buffering (must be >= max_num_ref_frames)
}

// SPSInfo is what parseSPS reads: enough to rewrite the SPS and to check the result.
type SPSInfo struct {
	ProfileIdc      uint8
	LevelIdc        uint8
	WidthMbs        uint32
	HeightMapUnits  uint32
	MaxNumRefFrames uint32
	HasVUI          bool
	// Only meaningful when the VUI has bitstream_restriction.
	HasBitstreamRestriction bool
	MaxNumReorderFrames     uint32
	MaxDecFrameBuffering    uint32
}

var errUnsupported = errors.New("h264: SPS uses a feature this parser does not handle")

// parseSPS reads an SPS RBSP (emulation prevention already removed, NAL header excluded)
// up to vui_parameters_present_flag and returns the bit position of that flag.
func parseSPS(rbsp []byte) (info SPSInfo, vuiFlagBit int, err error) {
	r := &bitReader{data: rbsp}
	profile, _ := r.u(8)
	r.u(8) // constraint_set flags + reserved
	level, _ := r.u(8)
	info.ProfileIdc, info.LevelIdc = uint8(profile), uint8(level)
	r.ue() // seq_parameter_set_id

	switch info.ProfileIdc {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma, _ := r.ue()
		if chroma == 3 {
			r.u(1) // separate_colour_plane_flag
		}
		r.ue() // bit_depth_luma_minus8
		r.ue() // bit_depth_chroma_minus8
		r.u(1) // qpprime_y_zero_transform_bypass_flag
		if scaling, _ := r.u(1); scaling == 1 {
			return info, 0, errUnsupported // seq_scaling_matrix_present_flag
		}
	}

	r.ue() // log2_max_frame_num_minus4
	pocType, _ := r.ue()
	switch pocType {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.u(1) // delta_pic_order_always_zero_flag
		r.se() // offset_for_non_ref_pic
		r.se() // offset_for_top_to_bottom_field
		cycle, _ := r.ue()
		if cycle > 255 {
			return info, 0, errUnsupported
		}
		for range cycle {
			r.se() // offset_for_ref_frame[i]
		}
	}
	info.MaxNumRefFrames, _ = r.ue()
	r.u(1) // gaps_in_frame_num_value_allowed_flag
	widthMinus1, _ := r.ue()
	heightMinus1, _ := r.ue()
	info.WidthMbs, info.HeightMapUnits = widthMinus1+1, heightMinus1+1
	if frameMbsOnly, _ := r.u(1); frameMbsOnly == 0 {
		r.u(1) // mb_adaptive_frame_field_flag
	}
	r.u(1) // direct_8x8_inference_flag
	if cropping, _ := r.u(1); cropping == 1 {
		r.ue() // frame_crop_left_offset
		r.ue() // frame_crop_right_offset
		r.ue() // frame_crop_top_offset
		r.ue() // frame_crop_bottom_offset
	}
	vuiFlagBit = r.pos
	vui, _ := r.u(1)
	info.HasVUI = vui == 1
	if r.err != nil {
		return info, 0, r.err
	}
	if info.HasVUI {
		info.parseVUIRestriction(r)
	}
	return info, vuiFlagBit, nil
}

// parseVUIRestriction reads a VUI far enough to find bitstream_restriction. It only
// supports VUIs without HRD parameters; others leave HasBitstreamRestriction false.
func (info *SPSInfo) parseVUIRestriction(r *bitReader) {
	if f, _ := r.u(1); f == 1 { // aspect_ratio_info_present_flag
		if idc, _ := r.u(8); idc == 255 { // Extended_SAR
			r.u(16)
			r.u(16)
		}
	}
	if f, _ := r.u(1); f == 1 { // overscan_info_present_flag
		r.u(1)
	}
	if f, _ := r.u(1); f == 1 { // video_signal_type_present_flag
		r.u(4)
		if c, _ := r.u(1); c == 1 { // colour_description_present_flag
			r.u(24)
		}
	}
	if f, _ := r.u(1); f == 1 { // chroma_loc_info_present_flag
		r.ue()
		r.ue()
	}
	if f, _ := r.u(1); f == 1 { // timing_info_present_flag
		r.u(32)
		r.u(32)
		r.u(1)
	}
	nalHRD, _ := r.u(1)
	vclHRD, _ := r.u(1)
	if nalHRD == 1 || vclHRD == 1 {
		return
	}
	r.u(1)                      // pic_struct_present_flag
	if f, _ := r.u(1); f == 1 { // bitstream_restriction_flag
		r.u(1) // motion_vectors_over_pic_boundaries_flag
		r.ue() // max_bytes_per_pic_denom
		r.ue() // max_bits_per_mb_denom
		r.ue() // log2_max_mv_length_horizontal
		r.ue() // log2_max_mv_length_vertical
		reorder, _ := r.ue()
		buffering, _ := r.ue()
		if r.err == nil {
			info.HasBitstreamRestriction = true
			info.MaxNumReorderFrames = reorder
			info.MaxDecFrameBuffering = buffering
		}
	}
}

// startCodeLen returns the length of the Annex B start code (3 or 4 bytes) at the start of nal.
func startCodeLen(nal []byte) (int, bool) {
	switch {
	case bytes.HasPrefix(nal, []byte{0, 0, 0, 1}):
		return 4, true
	case bytes.HasPrefix(nal, []byte{0, 0, 1}):
		return 3, true
	}
	return 0, false
}

// unescape removes emulation prevention bytes (00 00 03 → 00 00).
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// escape inserts emulation prevention bytes so no 00 00 0x (x <= 3) appears in the payload.
func escape(b []byte) []byte {
	out := make([]byte, 0, len(b)+4)
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
