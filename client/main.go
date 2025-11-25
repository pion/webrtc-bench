// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
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

func newPeerConnection() {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	gatherCompletePromise := webrtc.GatheringCompletePromise(peerConnection)
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	<-gatherCompletePromise

	offerJSON, err := json.Marshal(*peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	resp, err := http.Post(fmt.Sprintf("http://%s/doSignaling", os.Args[1]), "application/json", bytes.NewReader(offerJSON)) // nolint: lll, noctx
	if err != nil {
		panic(err)
	}
	resp.Close = true

	var answer webrtc.SessionDescription
	if err = json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		panic(err)
	}

	if err = peerConnection.SetRemoteDescription(answer); err != nil {
		panic(err)
	}
	if err = resp.Body.Close(); err != nil {
		panic(err)
	}
}

func main() {
	if len(os.Args) != 2 {
		panic("client expects server host+port")
	}

	for range time.NewTicker(5 * time.Second).C {
		for i := 0; i <= 10; i++ {
			newPeerConnection()
		}
	}
}
