// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/shirou/gopsutil/v4/cpu"
)

type latencyMeasurement struct {
	timestamp  time.Time // When the operation completed (not when it started)
	latencyMs  float64   // Latency duration in milliseconds
	metricType string    // Measurement type (see rawLatencyLogger for valid types)
}

// nolint: gochecknoglobals
var (
	outboundVideoTrack         *webrtc.TrackLocalStaticSample
	peerConnectionCount        int64
	rawLatencies               = make(chan latencyMeasurement, 10000)
	droppedLatencyMeasurements int64
)

// Generate latency measurements CSV with columns of timestamp, type, and latencyMs.
// The timestamp represents when the operation completed (not when it started).
//
// Valid measurement types (prefixed by protocol layer):
//   - "signaling_processing": Total server-side signaling processing time (offer to answer)
//   - "sdp_offer_processing": Time to process incoming SDP offer (SetRemoteDescription)
//   - "sdp_answer_creation": Time to create SDP answer (CreateAnswer)
//   - "ice_gathering": Time spent gathering local ICE candidates
//   - "ice_connection": Time from PeerConnection creation to ICE connected (includes signaling + network + client)
//   - "dtls_handshake": Time from ICE connected to DTLS handshake complete
func rawLatencyLogger() {
	file, err := os.OpenFile("server-latencies.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) // nolint: gosec
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
		case measurement, ok := <-rawLatencies:
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

// Generate CSV with columns of timestamp, peerConnectionCount, cpuUsage, and droppedLatencyMeasurements.
func reportBuilder() {
	file, err := os.OpenFile("report.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) // nolint: gosec
	if err != nil {
		panic(err)
	}

	if _, err := file.WriteString("timestamp, peerConnectionCount, cpuUsage, droppedLatencyMeasurements\n"); err != nil {
		panic(err)
	}

	for range time.NewTicker(3 * time.Second).C {
		usage, err := cpu.Percent(0, false)
		if err != nil {
			panic(err)
		} else if len(usage) != 1 {
			panic(fmt.Sprintf("CPU Usage results should have 1 sample, have %d", len(usage)))
		}
		if _, err = fmt.Fprintf(file, "%s, %d, %f, %d\n", time.Now().Format(time.RFC3339), atomic.LoadInt64(&peerConnectionCount), usage[0], atomic.LoadInt64(&droppedLatencyMeasurements)); err != nil { // nolint: lll
			panic(err)
		}
	}
}

// HTTP Handler that accepts an Offer and returns an Answer
// adds outboundVideoTrack to PeerConnection.
func doSignaling(w http.ResponseWriter, r *http.Request) { // nolint: cyclop, varnamelen
	signalingStartTime := time.Now()
	defer func() {
		latency := time.Since(signalingStartTime)
		measurement := latencyMeasurement{
			timestamp:  time.Now(),
			latencyMs:  float64(latency.Microseconds()) / 1000.0,
			metricType: "signaling_processing",
		}
		select {
		case rawLatencies <- measurement:
		default:
			// Channel full, drop measurement to avoid blocking
			atomic.AddInt64(&droppedLatencyMeasurements, 1)
		}
	}()

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	// Track when PeerConnection is created for ICE connection latency measurement
	peerConnectionCreatedTime := time.Now()

	// Track when ICE connects for DTLS handshake latency measurement
	var iceConnectedTime time.Time

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		switch connectionState { // nolint: exhaustive
		case webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateFailed:
			if closeErr := peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}
		case webrtc.ICEConnectionStateClosed:
			atomic.AddInt64(&peerConnectionCount, -1)
		case webrtc.ICEConnectionStateConnected:
			// Track when ICE connects for DTLS measurement
			iceConnectedTime = time.Now()

			// Measure ICE connection latency from PeerConnection creation to ICE connected.
			// NOTE: This includes signaling processing time, network RTT for client to receive answer,
			// client processing time, and ICE check exchanges. This is NOT a pure RTT measurement.
			iceLatency := time.Since(peerConnectionCreatedTime)
			iceMeasurement := latencyMeasurement{
				timestamp:  time.Now(),
				latencyMs:  float64(iceLatency.Microseconds()) / 1000.0,
				metricType: "ice_connection",
			}
			select {
			case rawLatencies <- iceMeasurement:
			default:
				// Channel full, drop measurement to avoid blocking
				atomic.AddInt64(&droppedLatencyMeasurements, 1)
			}

			atomic.AddInt64(&peerConnectionCount, 1)
		}
	})

	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		if connectionState == webrtc.PeerConnectionStateConnected && !iceConnectedTime.IsZero() {
			// Measure DTLS handshake latency (from ICE connected to PeerConnection connected)
			dtlsLatency := time.Since(iceConnectedTime)
			dtlsMeasurement := latencyMeasurement{
				timestamp:  time.Now(),
				latencyMs:  float64(dtlsLatency.Microseconds()) / 1000.0,
				metricType: "dtls_handshake",
			}
			select {
			case rawLatencies <- dtlsMeasurement:
			default:
				// Channel full, drop measurement to avoid blocking
				atomic.AddInt64(&droppedLatencyMeasurements, 1)
			}
		}
	})

	if _, err = peerConnection.AddTrack(outboundVideoTrack); err != nil {
		panic(err)
	}

	var offer webrtc.SessionDescription
	if err = json.NewDecoder(r.Body).Decode(&offer); err != nil {
		panic(err)
	}

	// Measure offer processing latency
	offerProcessingStartTime := time.Now()
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}
	offerProcessingLatency := time.Since(offerProcessingStartTime)
	offerProcessingMeasurement := latencyMeasurement{
		timestamp:  time.Now(),
		latencyMs:  float64(offerProcessingLatency.Microseconds()) / 1000.0,
		metricType: "sdp_offer_processing",
	}
	select {
	case rawLatencies <- offerProcessingMeasurement:
	default:
		// Channel full, drop measurement to avoid blocking
		atomic.AddInt64(&droppedLatencyMeasurements, 1)
	}

	gatherCompletePromise := webrtc.GatheringCompletePromise(peerConnection)

	// Measure answer creation latency
	answerCreationStartTime := time.Now()
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}
	answerCreationLatency := time.Since(answerCreationStartTime)
	answerCreationMeasurement := latencyMeasurement{
		timestamp:  time.Now(),
		latencyMs:  float64(answerCreationLatency.Microseconds()) / 1000.0,
		metricType: "sdp_answer_creation",
	}
	select {
	case rawLatencies <- answerCreationMeasurement:
	default:
		// Channel full, drop measurement to avoid blocking
		atomic.AddInt64(&droppedLatencyMeasurements, 1)
	}

	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Track when ICE gathering starts
	iceGatheringStartTime := time.Now()

	<-gatherCompletePromise

	// Measure ICE gathering latency
	iceGatheringLatency := time.Since(iceGatheringStartTime)
	iceGatheringMeasurement := latencyMeasurement{
		timestamp:  time.Now(),
		latencyMs:  float64(iceGatheringLatency.Microseconds()) / 1000.0,
		metricType: "ice_gathering",
	}
	select {
	case rawLatencies <- iceGatheringMeasurement:
	default:
		// Channel full, drop measurement to avoid blocking
		atomic.AddInt64(&droppedLatencyMeasurements, 1)
	}

	response, err := json.Marshal(*peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		panic(err)
	}
}

func main() {
	var err error
	outboundVideoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP8,
	}, "pion", "pion")
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			playVideo()
		}
	}()

	go reportBuilder()
	go rawLatencyLogger()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/doSignaling", doSignaling)

	fmt.Println("Open http://localhost:8080 to access this demo")
	panic(http.ListenAndServe(":8080", nil)) // nolint: gosec
}

const videoFileName = "input.ivf"

func playVideo() {
	file, err := os.Open(videoFileName)
	if err != nil {
		panic(err)
	}

	ivf, header, err := ivfreader.NewWith(file)
	if err != nil {
		panic(err)
	}

	ticker := time.NewTicker(
		time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000),
	)
	defer ticker.Stop()
	for ; true; <-ticker.C {
		frame, _, err := ivf.ParseNextFrame()
		if errors.Is(err, io.EOF) {
			return
		}

		if err != nil {
			panic(err)
		}

		if err = outboundVideoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
			panic(err)
		}
	}
}
