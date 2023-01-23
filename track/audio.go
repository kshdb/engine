package track

import (
	"m7s.live/engine/v4/codec"
	. "m7s.live/engine/v4/common"
	"m7s.live/engine/v4/util"
)

type Audio struct {
	Media
	CodecID    codec.AudioCodecID
	Channels   byte
	SampleSize byte
	AVCCHead   []byte // 音频包在AVCC格式中，AAC会有两个字节，其他的只有一个字节
	codec.AudioSpecificConfig
}

func (a *Audio) IsAAC() bool {
	return a.CodecID == codec.CodecID_AAC
}

func (a *Audio) Attach() {
	a.Stream.AddTrack(a)
	a.Attached = 1
}
func (a *Audio) Detach() {
	a.Stream.RemoveTrack(a)
	a.Attached = 2
}
func (a *Audio) GetName() string {
	if a.Name == "" {
		return a.CodecID.String()
	}
	return a.Name
}
func (a *Audio) GetInfo() *Audio {
	return a
}
func (av *Audio) WriteADTS(adts []byte) {

}
func (av *Audio) WriteRaw(pts uint32, raw []byte) {
	curValue := &av.Value
	curValue.BytesIn += len(raw)
	if len(av.AVCCHead) == 2 {
		raw = raw[7:] //AAC 去掉7个字节的ADTS头
	}
	curValue.AUList.PushItem(av.BytesPool.GetShell(raw))
	av.generateTimestamp(pts)
	av.Flush()
}

func (av *Audio) WriteAVCC(ts uint32, frame util.BLL) {
	av.Value.WriteAVCC(ts, frame)
	av.generateTimestamp(ts * 90)
	av.Flush()
}

func (a *Audio) CompleteAVCC(value *AVFrame) {
	value.AVCC.Push(a.BytesPool.GetShell(a.AVCCHead))
	for p := value.AUList.Head; p != nil; p = p.Next {
		for pp := p.Head; pp != nil; pp = pp.Next {
			value.AVCC.Push(a.BytesPool.GetShell(pp.Bytes))
		}
	}
}

func (a *Audio) CompleteRTP(value *AVFrame) {
	a.PacketizeRTP(value.AUList.ToList()...)
}
