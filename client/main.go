// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
)

type latencyMeasurement struct {
	timestamp  time.Time // When the operation completed (not when it started)
	latencyMs  float64   // Latency duration in milliseconds
	metricType string    // Measurement type (see latencyLogger for valid types)
}

// nolint: gochecknoglobals
var (
	clientLatencies = make(chan latencyMeasurement, 10000)
)

type connectionMetrics struct {
	// Timing milestones
	connectionStart      time.Time
	offerCreated         time.Time
	iceGatheringStart    time.Time
	iceGatheringComplete time.Time
	offerSent            time.Time
	answerReceived       time.Time
	iceConnected         time.Time
	dtlsComplete         time.Time
	mediaReady           time.Time
}

// Generate latency measurements CSV with columns of timestamp, type, and latencyMs.
// The timestamp represents when the operation completed (not when it started).
//
// Valid measurement types:
//   - "ice_gathering": Time spent gathering ICE candidates
//   - "signaling_rtt": Round-trip time from offer sent to answer received
//   - "ice_connection": Time from connection start to ICE connected
//   - "dtls_handshake": Time from ICE connected to DTLS handshake complete
//   - "media_ready": Total connection setup time from start to media ready
func latencyLogger() {
	file, err := os.OpenFile("client-latencies.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) // nolint: gosec
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			panic(err)
		}
	}()

	if _, err := file.WriteString("timestamp,type,latencyMs\n"); err != nil {
		panic(err)
	}

	syncTicker := time.NewTicker(3 * time.Second)
	defer syncTicker.Stop()

	for {
		select {
		case measurement, ok := <-clientLatencies:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(file, "%s,%s,%f\n",
				measurement.timestamp.Format(time.RFC3339Nano),
				measurement.metricType,
				measurement.latencyMs); err != nil {
				panic(err)
			}
		case <-syncTicker.C:
			if err := file.Sync(); err != nil {
				panic(err)
			}
		}
	}
}

func recordMetric(timestamp time.Time, metricType string, latencyMs float64) {
	measurement := latencyMeasurement{
		timestamp:  timestamp,
		latencyMs:  latencyMs,
		metricType: metricType,
	}
	select {
	case clientLatencies <- measurement:
	default:
		// Channel full, drop measurement to avoid blocking
	}
}

func recordConnectionMetrics(metrics *connectionMetrics) {
	// Calculate latencies
	iceGatheringLatency := float64(metrics.iceGatheringComplete.Sub(metrics.iceGatheringStart).Microseconds()) / 1000.0
	signalingRTT := float64(metrics.answerReceived.Sub(metrics.offerSent).Microseconds()) / 1000.0
	iceLatency := float64(metrics.iceConnected.Sub(metrics.connectionStart).Microseconds()) / 1000.0
	dtlsLatency := float64(metrics.dtlsComplete.Sub(metrics.iceConnected).Microseconds()) / 1000.0
	mediaReadyLatency := float64(metrics.mediaReady.Sub(metrics.connectionStart).Microseconds()) / 1000.0

	// Record to CSV via channel
	recordMetric(metrics.iceGatheringComplete, "ice_gathering", iceGatheringLatency)
	recordMetric(metrics.answerReceived, "signaling_rtt", signalingRTT)
	recordMetric(metrics.iceConnected, "ice_connection", iceLatency)
	recordMetric(metrics.dtlsComplete, "dtls_handshake", dtlsLatency)
	recordMetric(metrics.mediaReady, "media_ready", mediaReadyLatency)
}

func newPeerConnection() { // nolint: cyclop
	metrics := &connectionMetrics{
		connectionStart: time.Now(),
	}
	done := make(chan struct{})

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	// Track ICE connection state changes
	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected {
			metrics.iceConnected = time.Now()
		}
	})

	// Track when media is ready (ICE + DTLS complete)
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			metrics.dtlsComplete = time.Now()
			metrics.mediaReady = time.Now()

			// Record metrics after connection is established
			recordConnectionMetrics(metrics)
			close(done)
		}
	})

	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	gatherCompletePromise := webrtc.GatheringCompletePromise(peerConnection)

	metrics.offerCreated = time.Now()
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	metrics.iceGatheringStart = time.Now()
	<-gatherCompletePromise
	metrics.iceGatheringComplete = time.Now()

	offerJSON, err := json.Marshal(*peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	metrics.offerSent = time.Now()
	resp, err := http.Post(fmt.Sprintf("http://%s/doSignaling", os.Args[1]), "application/json", bytes.NewReader(offerJSON)) // nolint: lll, noctx
	if err != nil {
		panic(err)
	}
	resp.Close = true

	var answer webrtc.SessionDescription
	if err = json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		panic(err)
	}
	metrics.answerReceived = time.Now()

	if err = peerConnection.SetRemoteDescription(answer); err != nil {
		panic(err)
	}
	if err = resp.Body.Close(); err != nil {
		panic(err)
	}

	// Wait for connection to complete or timeout
	select {
	case <-done:
		// Connection completed successfully
	case <-time.After(5 * time.Second):
		// Connection timeout - metrics may be incomplete
		panic("connection timeout")
	}
}

func main() {
	if len(os.Args) != 2 {
		panic("client expects server host+port")
	}

	// Start latency logger goroutine
	go latencyLogger()

	for range time.NewTicker(5 * time.Second).C {
		for i := 0; i <= 10; i++ {
			newPeerConnection()
		}
	}
}
