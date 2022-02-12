package engine

import (
	"context"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/Monibuca/engine/v4/track"
	"github.com/Monibuca/engine/v4/util"
	. "github.com/logrusorgru/aurora"
	log "github.com/sirupsen/logrus"
)

type StreamState byte
type StreamAction byte

const (
	STATE_WAITPUBLISH StreamState = iota // 等待发布者状态
	STATE_WAITTRACK                      // 等待Track
	STATE_PUBLISHING                     // 正在发布流状态
	STATE_WAITCLOSE                      // 等待关闭状态(自动关闭延时开启)
	STATE_CLOSED                         // 流已关闭，不可使用
	STATE_DESTROYED                      // 资源已释放
)

const (
	ACTION_PUBLISH     StreamAction = iota
	ACTION_TIMEOUT                  // 发布流长时间没有数据/长时间没有发布者发布流/等待关闭时间到
	ACTION_PUBLISHLOST              // 发布者意外断开
	ACTION_CLOSE                    // 主动关闭流
	ACTION_LASTLEAVE                // 最后一个订阅者离开
	ACTION_FIRSTENTER               // 第一个订阅者进入
)

var StreamFSM = [STATE_DESTROYED + 1]map[StreamAction]StreamState{
	{
		ACTION_PUBLISH:   STATE_WAITTRACK,
		ACTION_TIMEOUT:   STATE_CLOSED,
		ACTION_LASTLEAVE: STATE_CLOSED,
		ACTION_CLOSE:     STATE_CLOSED,
	},
	{
		ACTION_PUBLISHLOST: STATE_WAITPUBLISH,
		ACTION_TIMEOUT:     STATE_PUBLISHING,
		ACTION_CLOSE:       STATE_CLOSED,
	},
	{
		ACTION_PUBLISHLOST: STATE_WAITPUBLISH,
		ACTION_TIMEOUT:     STATE_WAITPUBLISH,
		ACTION_LASTLEAVE:   STATE_WAITCLOSE,
		ACTION_CLOSE:       STATE_CLOSED,
	},
	{
		ACTION_PUBLISHLOST: STATE_CLOSED,
		ACTION_TIMEOUT:     STATE_CLOSED,
		ACTION_FIRSTENTER:  STATE_PUBLISHING,
		ACTION_CLOSE:       STATE_CLOSED,
	},
	{
		ACTION_TIMEOUT: STATE_DESTROYED,
	},
	{

	},
}

// Streams 所有的流集合
var Streams = util.Map[string, *Stream]{Map: make(map[string]*Stream)}

func FilterStreams[T IPublisher]() (ss []*Stream) {
	Streams.RLock()
	defer Streams.RUnlock()
	for _, s := range Streams.Map {
		if _, ok := s.Publisher.(T); ok {
			ss = append(ss, s)
		}
	}
	return
}

type UnSubscibeAction *Subscriber
type PublishAction struct{}
type UnPublishAction struct{}
type StreamTimeoutConfig struct {
	WaitTimeout      time.Duration
	PublishTimeout   time.Duration
	WaitCloseTimeout time.Duration
}

// Stream 流定义
type Stream struct {
	context.Context
	cancel context.CancelFunc
	StreamTimeoutConfig
	*url.URL
	Publisher   IPublisher
	State       StreamState
	timeout     *time.Timer //当前状态的超时定时器
	actionChan  chan any
	StartTime   time.Time               //流的创建时间
	Subscribers util.Slice[*Subscriber] // 订阅者
	Tracks
	FrameCount uint32 //帧总数
	AppName    string
	StreamName string
	*log.Entry `json:"-"`
}

func (s *Stream) SSRC() uint32 {
	return uint32(uintptr(unsafe.Pointer(s)))
}

func (s *Stream) UnPublish() {
	if !s.IsClosed() {
		s.actionChan <- UnPublishAction{}
	}
}

func findOrCreateStream(streamPath string, waitTimeout time.Duration) (s *Stream, created bool) {
	streamPath = strings.Trim(streamPath, "/")
	u, err := url.Parse(streamPath)
	if err != nil {
		return nil, false
	}
	p := strings.Split(u.Path, "/")
	if len(p) < 2 {
		log.Warn(Red("Stream Path Format Error:"), streamPath)
		return nil, false
	}
	if s, ok := Streams.Map[u.Path]; ok {
		s.Debug(Green("Stream Found"))
		return s, false
	} else {
		p := strings.Split(u.Path, "/")
		s = &Stream{
			URL:        u,
			AppName:    p[0],
			StreamName: util.LastElement(p),
			Entry:      log.WithField("stream", u.Path),
		}
		s.Info("created")
		s.WaitTimeout = waitTimeout
		Streams.Map[u.Path] = s
		s.actionChan = make(chan any, 1)
		s.StartTime = time.Now()
		s.timeout = time.NewTimer(waitTimeout)
		s.Context, s.cancel = context.WithCancel(Engine)
		s.Init(s)
		go s.run()
		return s, true
	}
}

func (r *Stream) action(action StreamAction) bool {
	r.Tracef("action:%d", action)
	if next, ok := StreamFSM[r.State][action]; ok {
		if r.Publisher != nil {
			// 给Publisher状态变更的回调，方便进行远程拉流等操作
			defer r.Publisher.OnStateChanged(r.State, next)
			if !r.Publisher.OnStateChange(r.State, next) {
				return false
			}
		}
		r.Debug(action, " :", r.State, "->", next)
		r.State = next
		switch next {
		case STATE_WAITPUBLISH:
			r.Publisher = nil
			Bus.Publish(Event_REQUEST_PUBLISH, r)
			r.timeout.Reset(r.WaitTimeout)
			if _, ok = PullOnSubscribeList[r.Path]; ok {
				PullOnSubscribeList[r.Path].Pull(r.Path)
			}
		case STATE_WAITTRACK:
			r.timeout.Reset(time.Second * 5)
		case STATE_PUBLISHING:
			r.WaitDone()
			r.timeout.Reset(r.PublishTimeout)
			Bus.Publish(Event_PUBLISH, r)
		case STATE_WAITCLOSE:
			r.timeout.Reset(r.WaitCloseTimeout)
		case STATE_CLOSED:
			r.cancel()
			if r.Publisher != nil {
				r.Publisher.Close()
			}
			r.WaitDone()
			Bus.Publish(Event_STREAMCLOSE, r)
			Streams.Delete(r.Path)
			r.timeout.Reset(time.Second) // 延迟1秒钟销毁，防止访问到已关闭的channel
		case STATE_DESTROYED:
			close(r.actionChan)
			fallthrough
		default:
			r.timeout.Stop()
		}
		return true
	}
	return false
}
func (r *Stream) IsClosed() bool {
	if r == nil {
		return true
	}
	return r.State >= STATE_CLOSED
}

func (r *Stream) Close() {
	if !r.IsClosed() {
		r.actionChan <- ACTION_CLOSE
	}
}

func (r *Stream) UnSubscribe(sub *Subscriber) {
	r.Debug("unsubscribe", sub.ID)
	if !r.IsClosed() {
		r.actionChan <- UnSubscibeAction(sub)
	}
}
func (r *Stream) Subscribe(sub *Subscriber) {
	r.Debug("subscribe", sub.ID)
	if !r.IsClosed() {
		sub.Stream = r
		sub.Context, sub.cancel = context.WithCancel(r)
		r.actionChan <- sub
	}
}

// 流状态处理中枢，包括接收订阅发布指令等
func (r *Stream) run() {
	var done = r.Done()
	for {
		select {
		case <-r.timeout.C:
			r.Debugf("%v timeout", r.State)
			r.action(ACTION_TIMEOUT)
		case <-done:
			r.action(ACTION_CLOSE)
			done = nil
		case action, ok := <-r.actionChan:
			if ok {
				switch v := action.(type) {
				case PublishAction:
					r.action(ACTION_PUBLISH)
				case UnPublishAction:
					r.action(ACTION_PUBLISHLOST)
				case StreamAction:
					r.action(v)
				case *Subscriber:
					r.Subscribers.Add(v)
					Bus.Publish(Event_SUBSCRIBE, v)
					v.Info(Sprintf(Yellow("added remains:%d") ,len(r.Subscribers)))
					if r.Subscribers.Len() == 1 {
						r.action(ACTION_FIRSTENTER)
					}
				case UnSubscibeAction:
					if r.Subscribers.Delete(v) {
						Bus.Publish(Event_UNSUBSCRIBE, v)
						(*Subscriber)(v).Info(Sprintf(Yellow("removed remains:%d"), len(r.Subscribers)))
						if r.Subscribers.Len() == 0 && r.WaitCloseTimeout > 0 {
							r.action(ACTION_LASTLEAVE)
						}
					}
				}
			} else {
				return
			}
		}
	}
}

// Update 更新数据重置超时定时器
func (r *Stream) Update() uint32 {
	if r.State == STATE_PUBLISHING {
		r.Trace("update")
		r.timeout.Reset(r.PublishTimeout)
	}
	return atomic.AddUint32(&r.FrameCount, 1)
}

// 如果暂时不知道编码格式可以用这个
func (r *Stream) NewVideoTrack() (vt *track.UnknowVideo) {
	r.Debug("create unknow video track")
	vt = &track.UnknowVideo{}
	vt.Stream = r
	return
}
func (r *Stream) NewAudioTrack() (at *track.UnknowAudio) {
	r.Debug("create unknow audio track")
	at = &track.UnknowAudio{}
	at.Stream = r
	return
}
func (r *Stream) NewH264Track() *track.H264 {
	r.Debug("create h264 track")
	return track.NewH264(r)
}

func (r *Stream) NewH265Track() *track.H265 {
	r.Debug("create h265 track")
	return track.NewH265(r)
}

func (r *Stream) NewAACTrack() *track.AAC {
	r.Debug("create aac track")
	return track.NewAAC(r)
}

// func (r *Stream) WaitDataTrack(names ...string) DataTrack {
// 	t := <-r.WaitTrack(names...)
// 	return t.(DataTrack)
// }
