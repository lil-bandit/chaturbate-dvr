package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r3labs/sse/v2"
	"github.com/teacat/chaturbate-dvr/channel"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/router/view"
	"github.com/teacat/chaturbate-dvr/server"
)

// Manager is responsible for managing channels and their states.
type Manager struct {
	Channels sync.Map
	SSE      *sse.Server
}

// New initializes a new Manager instance with an SSE server.
func New() (*Manager, error) {

	server := sse.New()
	server.SplitData = true

	updateStream := server.CreateStream("updates")
	updateStream.AutoReplay = false

	return &Manager{
		SSE: server,
	}, nil
}

// SaveConfig saves the current channels and state to a JSON file.
func (m *Manager) SaveConfig() error {
	var config []*entity.ChannelConfig

	m.Channels.Range(func(key, value any) bool {
		config = append(config, value.(*channel.Channel).Config)
		return true
	})

	b, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll("./conf", 0777); err != nil {
		return fmt.Errorf("mkdir all conf: %w", err)
	}
	if err := os.WriteFile("./conf/channels.json", b, 0777); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// LoadConfig loads the channels from JSON and starts them.
func (m *Manager) LoadConfig() error {
	b, err := os.ReadFile("./conf/channels.json")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	var config []*entity.ChannelConfig
	if err := json.Unmarshal(b, &config); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	for i, conf := range config {
		ch := channel.New(conf)
		m.Channels.Store(conf.Username, ch)

		if ch.Config.IsPaused {
			ch.Info("channel was paused, waiting for resume")
			continue
		}
		go ch.Resume(i)
	}
	return nil
}

// CreateChannel starts monitoring an M3U8 stream
func (m *Manager) CreateChannel(conf *entity.ChannelConfig, shouldSave bool) error {
	ch := channel.New(conf)

	// prevent duplicate channels
	_, ok := m.Channels.Load(conf.Username)
	if ok {
		return fmt.Errorf("channel %s already exists", conf.Username)
	}
	m.Channels.Store(conf.Username, ch)
	DownloadChannelImage(conf.Username)
	go ch.Resume(0)

	if shouldSave {
		if err := m.SaveConfig(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
	}
	return nil
}

// StopChannel stops the channel.
func (m *Manager) StopChannel(username string) error {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	thing.(*channel.Channel).Stop()
	m.Channels.Delete(username)

	if err := m.SaveConfig(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// PauseChannel pauses the channel.
func (m *Manager) PauseChannel(username string) error {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	thing.(*channel.Channel).Pause()

	if err := m.SaveConfig(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// ResumeChannel resumes the channel.
func (m *Manager) ResumeChannel(username string) error {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	go thing.(*channel.Channel).Resume(0)

	if err := m.SaveConfig(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// ChannelInfo returns a list of channel information for the web UI.
func (m *Manager) ChannelInfo() []*entity.ChannelInfo {
	var channels []*entity.ChannelInfo

	// Iterate over the channels and append their information to the slice
	m.Channels.Range(func(key, value any) bool {
		channels = append(channels, value.(*channel.Channel).ExportInfo())
		return true
	})

	sort.Slice(channels, func(i, j int) bool {
		return channels[i].CreatedAt > channels[j].CreatedAt
	})

	return channels
}

// Publish sends an SSE event to the specified channel.
func (m *Manager) Publish(evt entity.Event, info *entity.ChannelInfo) {
	switch evt {
	case entity.EventUpdate:
		var b bytes.Buffer
		if err := view.InfoTpl.ExecuteTemplate(&b, "channel_info", info); err != nil {
			fmt.Println("Error executing template:", err)
			return
		}
		m.SSE.Publish("updates", &sse.Event{
			Event: []byte(info.Username + "-info"),
			Data:  b.Bytes(),
		})
	case entity.EventLog:
		m.SSE.Publish("updates", &sse.Event{
			Event: []byte(info.Username + "-log"),
			Data:  []byte(strings.Join(info.Logs, "\n")),
		})
	}
}

// Subscriber handles SSE subscriptions for the specified channel.
func (m *Manager) Subscriber(w http.ResponseWriter, r *http.Request) {
	m.SSE.ServeHTTP(w, r)
}

func (m *Manager) SaveServerConfig() error {
	appCfg := entity.AppConfig{
		Framerate:      server.Config.Framerate,
		Resolution:     server.Config.Resolution,
		Pattern:        server.Config.Pattern,
		MaxDuration:    server.Config.MaxDuration,
		MaxFilesize:    server.Config.MaxFilesize,
		MaxConnections: server.Config.MaxConnections,
		Port:           server.Config.Port,
		Interval:       server.Config.Interval,
		Cookies:        server.Config.Cookies,
		UserAgent:      server.Config.UserAgent,
		Domain:         server.Config.Domain,
	}

	b, err := json.MarshalIndent(appCfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll("./conf", 0777); err != nil {
		return fmt.Errorf("mkdir all conf: %w", err)
	}
	if err := os.WriteFile("./conf/app.json", b, 0777); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

func (m *Manager) PreemptForPriority(newPriority int) (canStart bool) {
	max := server.Config.MaxConnections
	if max < 1 {
		return true // unlimited, always allow
	}

	var (
		count         int
		lowestPrio    = int(^uint(0) >> 1) // max int
		lowestChannel *channel.Channel
	)

	m.Channels.Range(func(key, value any) bool {
		ch := value.(*channel.Channel)
		if ch.IsOnline {
			count++
			if ch.Config.Priority < lowestPrio {
				lowestPrio = ch.Config.Priority
				lowestChannel = ch
			}
		}
		return true
	})

	if count < max {
		return true // slot available, no need to preempt
	}

	if newPriority > lowestPrio && lowestChannel != nil {
		lowestChannel.DownPrioritize()
		return true
	}

	return false
}

/* Not used */
func (m *Manager) UpdateAllChannels() {
	m.Channels.Range(func(key, value any) bool {
		ch := value.(*channel.Channel)
		if !ch.IsOnline {
			ch.Update()
		}
		return false
	})

}

/* Not used */
func (m *Manager) QueueDownPrioritizedChannels(username string) {
	m.Channels.Range(func(key, value any) bool {
		ch := value.(*channel.Channel)
		if ch.IsOnline && ch.IsDownPrioritized {
			ch.DownPrioritize()
		}
		return false
	})

}

func (m *Manager) GetChannelByUsername(username string) *entity.ChannelInfo {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	ch := thing.(*channel.Channel)
	return ch.ExportInfo() // assuming ExportInfo returns *entity.ChannelInfo
}

func (m *Manager) GetChannelRaw(username string) *channel.Channel {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	return thing.(*channel.Channel)
}

func DownloadChannelImage(username string, force ...bool) error {
	url := fmt.Sprintf("https://thumb.live.mmcdn.com/riw/%s.jpg?%d", username, time.Now().Unix())
	filepath := fmt.Sprintf("./conf/channel-images/%s.jpg", username)

	// Only skip download if force is not set or false
	if len(force) == 0 || !force[0] {
		if _, err := os.Stat(filepath); err == nil {
			// File exists, no need to download
			return nil
		}
	}

	if err := os.MkdirAll("./conf/channel-images", 0777); err != nil {
		return fmt.Errorf("mkdir all images: %w", err)
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}
