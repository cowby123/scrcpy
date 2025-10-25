package main

import (
	"log"
)

// sendNALUAccessUnitAtTS 以指定時間戳發送完整的 NALU 存取單元
func sendNALUAccessUnitAtTS(deviceIP string, nalus [][]byte, ts uint32) {
	if len(nalus) == 0 {
		return
	}

	clientsMu.RLock()
	clientList := make([]*ClientSession, 0, len(clients))
	for _, session := range clients {
		if session.DeviceIP == deviceIP {
			clientList = append(clientList, session)
		}
	}
	clientsMu.RUnlock()

	for _, session := range clientList {
		pk := session.Packetizer
		vt := session.VideoTrack
		if pk == nil || vt == nil {
			continue
		}

		for i, n := range nalus {
			if len(n) == 0 {
				continue
			}
			pkts := pk.Packetize(n, 0)
			for j, p := range pkts {
				p.Timestamp = ts
				p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
				if err := vt.WriteRTP(p); err != nil {
					log.Printf("[RTP][%s][設備:%s] write error: %v", session.ID, deviceIP, err)
				}
			}
		}
	}
}

// sendNALUsAtTS 以指定時間戳發送單獨的 NALU 單元
func sendNALUsAtTS(deviceIP string, ts uint32, nalus ...[]byte) {
	clientsMu.RLock()
	clientList := make([]*ClientSession, 0, len(clients))
	for _, session := range clients {
		if session.DeviceIP == deviceIP {
			clientList = append(clientList, session)
		}
	}
	clientsMu.RUnlock()

	for _, session := range clientList {
		pk := session.Packetizer
		vt := session.VideoTrack
		if pk == nil || vt == nil {
			continue
		}

		for _, n := range nalus {
			if len(n) == 0 {
				continue
			}
			pkts := pk.Packetize(n, 0)
			for _, p := range pkts {
				p.Timestamp = ts
				p.Marker = false
				if err := vt.WriteRTP(p); err != nil {
					log.Printf("[RTP][%s][設備:%s] write error: %v", session.ID, deviceIP, err)
				}
			}
		}
	}
}
