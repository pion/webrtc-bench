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

var (
	outboundVideoTrack  *webrtc.TrackLocalStaticSample
	peerConnectionCount int64
)

// Generate CSV with columns of timestamp, peerConnectionCount, and cpuUsage
func reportBuilder() {
	file, err := os.OpenFile("report.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		panic(err)
	}

	if _, err := file.WriteString("timestamp, peerConnectionCount, cpuUsage\n"); err != nil {
		panic(err)
	}

	for range time.NewTicker(3 * time.Second).C {
		usage, err := cpu.Percent(0, false)
		if err != nil {
			panic(err)
		} else if len(usage) != 1 {
			panic(fmt.Sprintf("CPU Usage results should have 1 sample, have %d", len(usage)))
		}
		if _, err = fmt.Fprintf(file, "%s, %d, %f\n", time.Now().Format(time.RFC3339), atomic.LoadInt64(&peerConnectionCount), usage[0]); err != nil {
			panic(err)
		}
	}
}

// HTTP Handler that accepts an Offer and returns an Answer
// adds outboundVideoTrack to PeerConnection
func doSignaling(w http.ResponseWriter, r *http.Request) {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		switch connectionState {
		case webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				panic(err)
			}
		case webrtc.ICEConnectionStateClosed:
			atomic.AddInt64(&peerConnectionCount, -1)
		case webrtc.ICEConnectionStateConnected:
			atomic.AddInt64(&peerConnectionCount, 1)
		}
	})

	if _, err = peerConnection.AddTrack(outboundVideoTrack); err != nil {
		panic(err)
	}

	var offer webrtc.SessionDescription
	if err = json.NewDecoder(r.Body).Decode(&offer); err != nil {
		panic(err)
	}

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	gatherCompletePromise := webrtc.GatheringCompletePromise(peerConnection)

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	<-gatherCompletePromise

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

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/doSignaling", doSignaling)

	fmt.Println("Open http://localhost:8080 to access this demo")
	panic(http.ListenAndServe(":8080", nil))
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
