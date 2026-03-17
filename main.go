package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	speech "cloud.google.com/go/speech/apiv1"
	speechpb "cloud.google.com/go/speech/apiv1/speechpb"
	"github.com/gorilla/websocket"
)

const (
	defaultPort      = 8027
	streamingLimit   = 290 * time.Second
	silenceInterval  = time.Second
	silenceAfter     = 2 * time.Second
	silenceByteCount = 3200
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	defaultConfig = &speechpb.StreamingRecognitionConfig{
		Config: &speechpb.RecognitionConfig{
			Encoding:                   speechpb.RecognitionConfig_LINEAR16,
			SampleRateHertz:            16000,
			LanguageCode:               "en-IN",
			Model:                      "latest_long",
			EnableAutomaticPunctuation: false,
		},
		InterimResults: true,
	}

	roidCounter uint64
	iosCounter  uint64
)

type controlMessage struct {
	Type       string          `json:"type,omitempty"`
	ClientID   string          `json:"clientId,omitempty"`
	ClientType string          `json:"clientType,omitempty"`
	Transcript string          `json:"transcript,omitempty"`
	IsFinal    bool            `json:"isFinal,omitempty"`
	Status     string          `json:"status,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Config     *configOverride `json:"config,omitempty"`
}

type configOverride struct {
	InterimResults *bool               `json:"interimResults,omitempty"`
	Config         *recognitionPayload `json:"config,omitempty"`
}

type recognitionPayload struct {
	Encoding                   string `json:"encoding,omitempty"`
	SampleRateHertz            int32  `json:"sampleRateHertz,omitempty"`
	LanguageCode               string `json:"languageCode,omitempty"`
	Model                      string `json:"model,omitempty"`
	EnableAutomaticPunctuation *bool  `json:"enableAutomaticPunctuation,omitempty"`
}

type connState struct {
	speechClient *speech.Client
	ws           *websocket.Conn
	clientID     string

	writeMu sync.Mutex
	mu      sync.Mutex

	config      *speechpb.StreamingRecognitionConfig
	stream      speechpb.Speech_StreamingRecognizeClient
	streamCtx   context.Context
	cancel      context.CancelFunc
	lastAudio   time.Time
	restart     *time.Timer
	silenceTick *time.Ticker
	closed      bool
}

func main() {
	port := defaultPort
	if raw := os.Getenv("PORT"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			port = parsed
		}
	}

	client, err := speech.NewClient(context.Background())
	if err != nil {
		log.Fatalf("create speech client: %v", err)
	}
	defer client.Close()

	server := http.NewServeMux()
	server.HandleFunc("/health", healthHandler)
	server.HandleFunc("/stt-proxy-server/health", healthHandler)
	server.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.NotFound(w, r)
			return
		}
		handleWS(client, w, r)
	})

	log.Printf("[Proxy] Updated Listening on port %d", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), server))
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func handleWS(client *speech.Client, w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Proxy] WebSocket upgrade failed: %v", err)
		return
	}

	userAgent := r.UserAgent()
	clientType := detectClientType(userAgent)
	clientID := assignClientID(clientType)
	log.Printf("[Proxy] Client connected - %s (%s, UA: %s)", clientID, clientType, userAgent)

	c := &connState{
		speechClient: client,
		ws:           ws,
		clientID:     clientID,
		config:       cloneConfig(defaultConfig),
		lastAudio:    time.Now(),
	}
	defer c.close()

	if err := c.sendJSON(controlMessage{
		Type:       "connected",
		ClientID:   clientID,
		ClientType: clientType,
	}); err != nil {
		log.Printf("[Proxy][%s] send connected message: %v", clientID, err)
		return
	}

	if err := c.ensureStream(); err != nil {
		log.Printf("[Proxy][%s] initialize stream: %v", clientID, err)
		_ = c.sendJSON(controlMessage{
			Type:     "grpc_status",
			ClientID: clientID,
			Status:   "error",
			Reason:   err.Error(),
		})
		return
	}

	c.startSilenceLoop()

	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				log.Printf("[Proxy][%s] Client disconnected - code: %d, reason: %s", clientID, closeErr.Code, closeErr.Text)
			} else {
				log.Printf("[Proxy][%s] WebSocket error: %v", clientID, err)
			}
			return
		}

		switch messageType {
		case websocket.TextMessage:
			c.handleText(data)
		case websocket.BinaryMessage:
			c.handleAudio(data)
		}
	}
}

func (c *connState) handleText(data []byte) {
	var msg controlMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "config":
		c.mu.Lock()
		c.config = mergeConfig(defaultConfig, msg.Config)
		configJSON, _ := json.Marshal(c.config.GetConfig())
		c.mu.Unlock()

		log.Printf("[Proxy][%s] Config updated: %s", c.clientID, string(configJSON))
		if err := c.restartStream(); err != nil {
			log.Printf("[Proxy][%s] restart after config: %v", c.clientID, err)
			_ = c.sendJSON(controlMessage{
				Type:     "grpc_status",
				ClientID: c.clientID,
				Status:   "error",
				Reason:   err.Error(),
			})
			return
		}

		_ = c.sendJSON(controlMessage{
			Type:     "config_applied",
			ClientID: c.clientID,
		})
	case "commit":
		log.Printf("[Proxy][%s] Commit received - flushing gRPC stream", c.clientID)
		if err := c.restartStream(); err != nil {
			log.Printf("[Proxy][%s] restart after commit: %v", c.clientID, err)
			_ = c.sendJSON(controlMessage{
				Type:     "grpc_status",
				ClientID: c.clientID,
				Status:   "error",
				Reason:   err.Error(),
			})
		}
	}
}

func (c *connState) handleAudio(data []byte) {
	if len(data) == 0 {
		return
	}

	c.mu.Lock()
	c.lastAudio = time.Now()
	stream := c.stream
	c.mu.Unlock()

	if stream == nil {
		if err := c.ensureStream(); err != nil {
			log.Printf("[Proxy][%s] get stream for audio: %v", c.clientID, err)
			return
		}
		c.mu.Lock()
		stream = c.stream
		c.mu.Unlock()
	}

	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
			AudioContent: data,
		},
	}); err != nil {
		log.Printf("[Proxy][%s] Error writing to stream: %v", c.clientID, err)
	}
}

func (c *connState) ensureStream() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.stream != nil {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.speechClient.StreamingRecognize(ctx)
	if err != nil {
		cancel()
		return err
	}

	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: cloneConfig(c.config),
		},
	}); err != nil {
		cancel()
		return err
	}

	c.stream = stream
	c.streamCtx = ctx
	c.cancel = cancel
	c.scheduleRestartLocked()
	go c.readResponses(stream, ctx)
	return nil
}

func (c *connState) readResponses(stream speechpb.Speech_StreamingRecognizeClient, ctx context.Context) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			c.handleStreamEnd(stream, ctx, err)
			return
		}

		results := resp.GetResults()
		if len(results) == 0 || len(results[0].GetAlternatives()) == 0 {
			continue
		}

		transcript := results[0].GetAlternatives()[0].GetTranscript()
		if transcript == "" {
			continue
		}

		isFinal := results[0].GetIsFinal()
		log.Printf("[gRPC][%s] Transcript: %q (final: %t)", c.clientID, transcript, isFinal)
		if err := c.sendJSON(controlMessage{
			ClientID:   c.clientID,
			Transcript: transcript,
			IsFinal:    isFinal,
		}); err != nil {
			log.Printf("[Proxy][%s] send transcript: %v", c.clientID, err)
			return
		}
	}
}

func (c *connState) handleStreamEnd(stream speechpb.Speech_StreamingRecognizeClient, ctx context.Context, err error) {
	c.mu.Lock()
	current := c.stream == stream && c.streamCtx == ctx
	if current {
		c.stream = nil
		c.streamCtx = nil
		c.cancel = nil
	}
	closed := c.closed
	c.mu.Unlock()

	if err == nil || err == io.EOF || errorsAreCanceled(err) {
		if !closed {
			log.Printf("[gRPC][%s] gRPC stream ended", c.clientID)
			_ = c.sendJSON(controlMessage{
				Type:     "grpc_status",
				ClientID: c.clientID,
				Status:   "ended",
			})
		}
		return
	}

	if !closed {
		log.Printf("[gRPC][%s] gRPC stream error: %v", c.clientID, err)
		_ = c.sendJSON(controlMessage{
			Type:     "grpc_status",
			ClientID: c.clientID,
			Status:   "error",
			Reason:   err.Error(),
		})
	}
}

func (c *connState) restartStream() error {
	c.mu.Lock()
	stream := c.stream
	cancel := c.cancel
	c.stream = nil
	c.streamCtx = nil
	c.cancel = nil
	if c.restart != nil {
		c.restart.Stop()
		c.restart = nil
	}
	c.mu.Unlock()

	if stream != nil {
		_ = stream.CloseSend()
	}
	if cancel != nil {
		cancel()
	}

	return c.ensureStream()
}

func (c *connState) scheduleRestartLocked() {
	if c.restart != nil {
		c.restart.Stop()
	}

	c.restart = time.AfterFunc(streamingLimit, func() {
		log.Printf("[Proxy][%s] 290s limit reached. Seamlessly restarting gRPC stream.", c.clientID)
		if err := c.restartStream(); err != nil {
			log.Printf("[Proxy][%s] scheduled restart failed: %v", c.clientID, err)
		}
	})
}

func (c *connState) startSilenceLoop() {
	c.mu.Lock()
	if c.silenceTick != nil {
		c.mu.Unlock()
		return
	}
	tick := time.NewTicker(silenceInterval)
	c.silenceTick = tick
	c.mu.Unlock()

	go func() {
		silence := make([]byte, silenceByteCount)
		for range tick.C {
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			lastAudio := c.lastAudio
			stream := c.stream
			c.mu.Unlock()

			if stream == nil || time.Since(lastAudio) < silenceAfter {
				continue
			}

			if err := stream.Send(&speechpb.StreamingRecognizeRequest{
				StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
					AudioContent: silence,
				},
			}); err != nil {
				log.Printf("[Proxy][%s] silence write failed: %v", c.clientID, err)
			}
		}
	}()
}

func (c *connState) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	stream := c.stream
	cancel := c.cancel
	c.stream = nil
	c.streamCtx = nil
	c.cancel = nil
	if c.restart != nil {
		c.restart.Stop()
	}
	if c.silenceTick != nil {
		c.silenceTick.Stop()
	}
	c.mu.Unlock()

	if stream != nil {
		_ = stream.CloseSend()
	}
	if cancel != nil {
		cancel()
	}
	_ = c.ws.Close()
}

func (c *connState) sendJSON(msg controlMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteJSON(msg)
}

func detectClientType(userAgent string) string {
	log.Printf("[Proxy] Client type : %s", userAgent)
	ua := strings.ToLower(userAgent)
	if strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad") || strings.Contains(ua, "ios") {
		return "ios"
	}
	return "roid"
}

func assignClientID(clientType string) string {
	if clientType == "ios" {
		return fmt.Sprintf("ios%03d", atomic.AddUint64(&iosCounter, 1))
	}
	return fmt.Sprintf("roid%03d", atomic.AddUint64(&roidCounter, 1))
}

func cloneConfig(src *speechpb.StreamingRecognitionConfig) *speechpb.StreamingRecognitionConfig {
	if src == nil {
		return &speechpb.StreamingRecognitionConfig{}
	}

	out := &speechpb.StreamingRecognitionConfig{
		InterimResults:  src.GetInterimResults(),
		SingleUtterance: src.GetSingleUtterance(),
	}
	if cfg := src.GetConfig(); cfg != nil {
		out.Config = &speechpb.RecognitionConfig{
			Encoding:                   cfg.GetEncoding(),
			SampleRateHertz:            cfg.GetSampleRateHertz(),
			LanguageCode:               cfg.GetLanguageCode(),
			Model:                      cfg.GetModel(),
			EnableAutomaticPunctuation: cfg.GetEnableAutomaticPunctuation(),
		}
	}
	return out
}

func mergeConfig(base *speechpb.StreamingRecognitionConfig, override *configOverride) *speechpb.StreamingRecognitionConfig {
	out := cloneConfig(base)
	if override == nil {
		return out
	}
	if override.InterimResults != nil {
		out.InterimResults = *override.InterimResults
	}
	if out.Config == nil {
		out.Config = &speechpb.RecognitionConfig{}
	}
	if override.Config == nil {
		return out
	}

	if strings.EqualFold(override.Config.Encoding, "LINEAR16") {
		out.Config.Encoding = speechpb.RecognitionConfig_LINEAR16
	}
	if override.Config.SampleRateHertz != 0 {
		out.Config.SampleRateHertz = override.Config.SampleRateHertz
	}
	if override.Config.LanguageCode != "" {
		out.Config.LanguageCode = override.Config.LanguageCode
	}
	if override.Config.Model != "" {
		out.Config.Model = override.Config.Model
	}
	if override.Config.EnableAutomaticPunctuation != nil {
		out.Config.EnableAutomaticPunctuation = *override.Config.EnableAutomaticPunctuation
	}
	return out
}

func errorsAreCanceled(err error) bool {
	if err == context.Canceled {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "canceled")
}
