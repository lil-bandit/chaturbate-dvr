package channel

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Channel represents a channel instance.
type Channel struct {
	CancelFunc context.CancelFunc
	LogCh      chan string
	UpdateCh   chan bool

	IsOnline          bool
	IsDownPrioritized bool
	IsBlocked         bool // Used for blocking channels
	StreamedAt        int64
	Duration          float64 // Seconds
	Filesize          int     // Bytes
	Sequence          int

	Logs []string

	File   *os.File
	Config *entity.ChannelConfig

	//PublishFunc func(event string, data *entity.ChannelInfo)
}

// New creates a new channel instance with the given manager and configuration.
func New(conf *entity.ChannelConfig) *Channel {
	ch := &Channel{
		LogCh:      make(chan string),
		UpdateCh:   make(chan bool),
		Config:     conf,
		CancelFunc: func() {},
	}
	go ch.Publisher()

	return ch
}

// Publisher listens for log messages and updates from the channel
// and publishes once received.
func (ch *Channel) Publisher() {
	for {
		select {
		case v := <-ch.LogCh:
			// Append the log message to ch.Logs and keep only the last 100 rows
			ch.Logs = append(ch.Logs, v)
			if len(ch.Logs) > 100 {
				ch.Logs = ch.Logs[len(ch.Logs)-100:]
			}
			server.Manager.Publish(entity.EventLog, ch.ExportInfo())

		case <-ch.UpdateCh:
			server.Manager.Publish(entity.EventUpdate, ch.ExportInfo())
		}
	}
}

// WithCancel creates a new context with a cancel function,
// then stores the cancel function in the channel's CancelFunc field.
//
// This is used to cancel the context when the channel is stopped or paused.
func (ch *Channel) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, ch.CancelFunc = context.WithCancel(ctx)
	log.Printf(" INFO [%s] %s", ch.Config.Username, "STOP")
	return ctx, ch.CancelFunc
}

// Info logs an informational message.
func (ch *Channel) Info(format string, a ...any) {
	ch.LogCh <- fmt.Sprintf("%s [INFO] %s", time.Now().Format("15:04"), fmt.Sprintf(format, a...))
	log.Printf(" INFO [%s] %s", ch.Config.Username, fmt.Sprintf(format, a...))
}

// Error logs an error message.
func (ch *Channel) Error(format string, a ...any) {
	ch.LogCh <- fmt.Sprintf("%s [ERROR] %s", time.Now().Format("15:04"), fmt.Sprintf(format, a...))
	log.Printf("ERROR [%s] %s", ch.Config.Username, fmt.Sprintf(format, a...))
}

// ExportInfo exports the channel information as a ChannelInfo struct.
func (ch *Channel) ExportInfo() *entity.ChannelInfo {
	var filename string
	if ch.File != nil {
		filename = ch.File.Name()
	}
	var streamedAt string
	if ch.StreamedAt != 0 {
		streamedAt = time.Unix(ch.StreamedAt, 0).Format("2006-01-02 15:04 AM")
	}
	return &entity.ChannelInfo{
		IsOnline:          ch.IsOnline,
		IsPaused:          ch.Config.IsPaused,
		IsDownPrioritized: ch.IsDownPrioritized,
		IsBlocked:         ch.IsBlocked,
		Username:          ch.Config.Username,
		MaxDuration:       internal.FormatDuration(float64(ch.Config.MaxDuration * 60)), // MaxDuration from config is in minutes
		MaxFilesize:       internal.FormatFilesize(ch.Config.MaxFilesize * 1024 * 1024), // MaxFilesize from config is in MB
		//MinFilesize:       internal.FormatFilesize(ch.Config.MinFilesize * 1024 * 1024), // MinFilesize from config is in MB
		StreamedAt: streamedAt,
		CreatedAt:  ch.Config.CreatedAt,
		Duration:   internal.FormatDuration(ch.Duration),
		Filesize:   internal.FormatFilesize(ch.Filesize),
		//DurationInt:    ch.Duration,
		FilesizeBytes:  fmt.Sprintf("%d", ch.Filesize),
		Priority:       ch.Config.Priority,
		Filename:       filename,
		Logs:           ch.Logs,
		GlobalConfig:   server.Config,
		MaxFilesizeInt: ch.Config.MaxFilesize,
		MaxDurationInt: ch.Config.MaxDuration,
		Framerate:      ch.Config.Framerate,
		Resolution:     ch.Config.Resolution,
		Pattern:        ch.Config.Pattern,
	}
}

// Recycle pauses the channel and cancels the context.
// This is called when a channel with higher priority is online, and MaxConnections is reached.

func (ch *Channel) DownPrioritize() {
	// Stop the monitoring loop
	ch.CancelFunc()
	ch.IsDownPrioritized = true
	ch.Sequence = 0
	ch.IsOnline = false

	time_delay := time.Duration(server.Config.Interval) * time.Minute
	ch.Update()
	ch.Info("Downprioritized [%s]", ch.Config.Username)

	<-time.After(time_delay) // Wait for the interval before starting the monitoring again

	// Resume the channel monitoring after the delay
	go ch.Monitor()
}

// Pause pauses the channel and cancels the context.
func (ch *Channel) Pause() {
	// Stop the monitoring loop
	ch.CancelFunc()

	ch.Config.IsPaused = true
	ch.Sequence = 0
	ch.IsOnline = false

	ch.Update()
	ch.Info("channel paused")
}

// Stop stops the channel and cancels the context.
func (ch *Channel) Stop() {
	// Stop the monitoring loop
	ch.CancelFunc()

	ch.Info("channel stopped")
}

// Resume resumes the channel monitoring.
//
// `startSeq` is used to prevent all channels from starting at the same time, preventing TooManyRequests errors.
// It's only be used when program starting and trying to resume all channels at once.
func (ch *Channel) Resume(startSeq int) {
	ch.Config.IsPaused = false

	ch.Update()
	ch.Info("channel resumed")

	<-time.After(time.Duration(startSeq) * time.Second)
	go ch.Monitor()
}
