package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/lonng/nano"
	"github.com/lonng/nano/component"
	"github.com/lonng/nano/pipeline"
	"github.com/lonng/nano/scheduler"
	"github.com/lonng/nano/serialize/json"
	"github.com/lonng/nano/session"
	"github.com/panjf2000/ants/v2"
)

type (
	Room struct {
		group *nano.Group
	}

	// RoomManager represents a component that contains a bundle of room
	RoomManager struct {
		component.Base
		timer *scheduler.Timer
		rooms map[int]*Room
	}

	// UserMessage represents a message that user sent
	UserMessage struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}

	// NewUser message will be received when new user join room
	NewUser struct {
		Content string `json:"content"`
	}

	// AllMembers contains all members uid
	AllMembers struct {
		Members []int64 `json:"members"`
	}

	// JoinResponse represents the result of joining room
	JoinResponse struct {
		Code   int    `json:"code"`
		Result string `json:"result"`
	}

	stats struct {
		component.Base
		timer         *scheduler.Timer
		outboundBytes int
		inboundBytes  int
	}
)

var mySchedulerInstance scheduler.LocalScheduler

func (stats *stats) outbound(s *session.Session, msg *pipeline.Message) error {
	stats.outboundBytes += len(msg.Data)
	return nil
}

func (stats *stats) inbound(s *session.Session, msg *pipeline.Message) error {
	stats.inboundBytes += len(msg.Data)
	return nil
}

func (stats *stats) AfterInit() {
	stats.timer = scheduler.NewTimer(time.Minute, func() {
		println("OutboundBytes", stats.outboundBytes)
		println("InboundBytes", stats.outboundBytes)
	})
}

const (
	testRoomID = 1
	roomIDKey  = "ROOM_ID"
)

func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms: map[int]*Room{},
	}
}

// AfterInit component lifetime callback
func (mgr *RoomManager) AfterInit() {
	session.Lifetime.OnClosed(func(s *session.Session) {
		if !s.HasKey(roomIDKey) {
			return
		}
		room := s.Value(roomIDKey).(*Room)
		room.group.Leave(s)
	})
	mgr.timer = scheduler.NewTimer(time.Minute, func() {
		for roomId, room := range mgr.rooms {
			println(fmt.Sprintf("UserCount: RoomID=%d, Time=%s, Count=%d",
				roomId, time.Now().String(), room.group.Count()))
		}
	})
}

// Join room
func (mgr *RoomManager) Join(s *session.Session, msg []byte) error {
	// NOTE: join test room only in demo
	room, found := mgr.rooms[testRoomID]
	if !found {
		room = &Room{
			group: nano.NewGroup(fmt.Sprintf("room-%d", testRoomID)),
		}
		mgr.rooms[testRoomID] = room
	}

	fakeUID := s.ID() // just use s.ID as uid !!!
	s.Bind(fakeUID)   // binding session uids.Set(roomIDKey, room)
	s.Set(roomIDKey, room)
	s.Push("onMembers", &AllMembers{Members: room.group.Members()})
	// notify others
	room.group.Broadcast("onNewUser", &NewUser{Content: fmt.Sprintf("New user: %d", s.ID())})
	// new user join group
	room.group.Add(s) // add session to group
	return s.Response(&JoinResponse{Result: "success"})
}

// Message sync last message to all members
func (mgr *RoomManager) Message(s *session.Session, msg *UserMessage) error {
	if !s.HasKey(roomIDKey) {
		return fmt.Errorf("not join room yet")
	}
	room := s.Value(roomIDKey).(*Room)
	return room.group.Broadcast("onMessage", msg)
}

func main() {
	components := &component.Components{}
	components.Register(
		NewRoomManager(),
		component.WithName("room"), // rewrite component and handler name
		component.WithNameFunc(strings.ToLower),
		component.WithSchedulerName("my-custom-scheduler"), // 设置调度器名
	)

	var err error
	mySchedulerInstance, err = NewLocalScheduler()
	if err != nil {
		log.Fatalf("Failed to create local scheduler: %v", err)
	}

	// traffic stats
	pip := pipeline.New()
	stats := &stats{}
	pip.Outbound().PushBack(stats.outbound)
	pip.Inbound().PushBack(stats.inbound)

	log.SetFlags(log.LstdFlags | log.Llongfile)
	http.Handle("/web/", http.StripPrefix("/web/", http.FileServer(http.Dir("web"))))

	nano.Listen(":3250",
		nano.WithIsWebsocket(true),
		nano.WithPipeline(pip),
		nano.WithCheckOriginFunc(func(_ *http.Request) bool { return true }),
		nano.WithWSPath("/nano"),
		nano.WithDebugMode(),
		nano.WithSerializer(json.NewSerializer()), // override default serializer
		nano.WithComponents(components),
		nano.WithHandshakeValidator(customHandshakeValidator),
	)
}

// localScheduler is the concrete implementation of the LocalScheduler interface.
// It uses an ants.Pool to manage a pool of goroutines.
type localScheduler struct {
	pool *ants.Pool
}

// options holds the configurable parameters for the scheduler's ants pool.
type options struct {
	// Size is the maximum number of goroutines in the pool.
	Size int
	// ExpiryDuration is the period for which an idle goroutine is kept alive.
	ExpiryDuration time.Duration
	// PreAlloc indicates if memory for goroutines should be pre-allocated.
	PreAlloc bool
	// Logger is the custom logger for the ants pool.
	Logger ants.Logger
	// PanicHandler is the function to handle panics from tasks.
	PanicHandler func(interface{})
}

// Option is a function that configures the scheduler's options.
type Option func(*options)

// WithSize sets the maximum number of goroutines in the pool.
// The default is ants.DefaultAntsPoolSize.
func WithSize(size int) Option {
	return func(o *options) {
		o.Size = size
	}
}

// WithExpiryDuration sets the duration for which an idle goroutine is kept alive.
// The default is 10 seconds.
func WithExpiryDuration(duration time.Duration) Option {
	return func(o *options) {
		o.ExpiryDuration = duration
	}
}

// WithPreAlloc enables or disables pre-allocation of memory for goroutines.
func WithPreAlloc(preAlloc bool) Option {
	return func(o *options) {
		o.PreAlloc = preAlloc
	}
}

// WithLogger sets a custom logger for the pool.
// The default is a standard log.Logger.
func WithLogger(logger ants.Logger) Option {
	return func(o *options) {
		o.Logger = logger
	}
}

// WithPanicHandler sets a function to handle panics from tasks.
// If not set, panics will be propagated.
func WithPanicHandler(handler func(interface{})) Option {
	return func(o *options) {
		o.PanicHandler = handler
	}
}

// NewLocalScheduler creates and returns a new LocalScheduler instance.
// It initializes an ants pool based on the provided configuration options.
func NewLocalScheduler(opts ...Option) (scheduler.LocalScheduler, error) {
	// 1. Define default options.
	defaultOpts := &options{
		Size:           ants.DefaultAntsPoolSize,
		ExpiryDuration: 10 * time.Second,
		PreAlloc:       false,
		Logger:         log.Default(),
	}

	// 2. Apply user-provided options to override defaults.
	for _, opt := range opts {
		opt(defaultOpts)
	}

	// 3. Create the ants pool with the configured options.
	pool, err := ants.NewPool(
		defaultOpts.Size,
		ants.WithExpiryDuration(defaultOpts.ExpiryDuration),
		ants.WithPreAlloc(defaultOpts.PreAlloc),
		ants.WithLogger(defaultOpts.Logger),
		ants.WithPanicHandler(defaultOpts.PanicHandler),
	)
	if err != nil {
		return nil, err
	}

	return &localScheduler{pool: pool}, nil
}

// Schedule submits a task to the ants goroutine pool for execution.
// It adheres to the LocalScheduler interface.
func (s *localScheduler) Schedule(task scheduler.Task) {
	err := s.pool.Submit(task)
	if err != nil {
		log.Printf("Failed to submit task to scheduler: %v", err)
	}
}

// Release gracefully stops the goroutine pool.
func (s *localScheduler) Release() {
	s.pool.Release()
}

// 自定义握手验证器示例
func customHandshakeValidator(s *session.Session, data []byte) error {
	s.Set("my-custom-scheduler", mySchedulerInstance) // set custom scheduler instance

	fmt.Println("==================Custom handshake validator called=================")

	return nil
}
