package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/zcl"
)

// The Time cluster is one of the few a coordinator is expected to serve rather
// than consume. Devices read it to timestamp their own data, and a device that
// gets no answer keeps asking — this sensor retried every two seconds, which
// on a battery device is not free.
//
// Advertising cluster 0x000A as an input while ignoring the reads is the worst
// of both worlds, so gzb answers them.

// zigbeeEpoch is the origin of the ZCL UTCTime type: 2000-01-01 00:00:00 UTC,
// not the Unix epoch.
var zigbeeEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Time cluster attribute IDs.
const (
	attrTime       uint16 = 0x0000
	attrTimeStatus uint16 = 0x0001
	attrTimeZone   uint16 = 0x0002
	attrDstStart   uint16 = 0x0003
	attrDstEnd     uint16 = 0x0004
	attrDstShift   uint16 = 0x0005
	attrLocalTime  uint16 = 0x0007
)

// timeStatusMaster marks this node as a time master whose clock is
// authoritative and synchronised.
const timeStatusMaster uint8 = 0x03

// timeAttribute answers a single Time cluster attribute for the given instant.
//
// DST is folded into the timezone offset rather than modelled with transition
// times: the offset reported is whatever is in force now, and the DST fields
// are reported as zero. That keeps LocalTime correct, which is the only thing
// devices actually use, without pretending to know future transitions.
func timeAttribute(id uint16, now time.Time) (zcl.Record, bool) {
	utc := uint64(now.UTC().Sub(zigbeeEpoch) / time.Second)
	_, offsetSeconds := now.Zone()

	switch id {
	case attrTime:
		return zcl.Record{ID: id, Type: zcl.TypeUTCTime, Value: utc}, true
	case attrTimeStatus:
		return zcl.Record{ID: id, Type: zcl.TypeBitmap8, Value: uint64(timeStatusMaster)}, true
	case attrTimeZone:
		return zcl.Record{ID: id, Type: zcl.TypeInt32, Value: int64(offsetSeconds)}, true
	case attrDstStart, attrDstEnd:
		return zcl.Record{ID: id, Type: zcl.TypeUint32, Value: uint64(0)}, true
	case attrDstShift:
		return zcl.Record{ID: id, Type: zcl.TypeInt32, Value: int64(0)}, true
	case attrLocalTime:
		return zcl.Record{ID: id, Type: zcl.TypeUint32, Value: utc + uint64(offsetSeconds)}, true
	default:
		return zcl.Record{ID: id, Status: zcl.StatusUnsupportedAttribute}, false
	}
}

// serveTimeRead answers a Read Attributes on the Time cluster.
//
// It reports whether the message was a time read it handled, so callers can
// tell "answered" from "not for me".
func serveTimeRead(ctx context.Context, conn *ezsp.Conn, msg ezsp.IncomingMessage, frame zcl.Frame) bool {
	if msg.APS.Cluster != zcl.ClusterTime || frame.Command != zcl.CmdReadAttributes {
		return false
	}
	ids, err := frame.ReadRequest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		return false
	}

	now := time.Now()
	records := make([]zcl.Record, 0, len(ids))
	for _, id := range ids {
		rec, _ := timeAttribute(id, now)
		records = append(records, rec)
	}

	payload, err := zcl.ReadAttributesResponse(frame.Sequence, records)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gzb: building time response: %v\n", err)
		return false
	}

	// Reply to the endpoint that asked, from the endpoint it addressed.
	reply := ezsp.APSFrame{
		Profile:  msg.APS.Profile,
		Cluster:  msg.APS.Cluster,
		SourceEP: msg.APS.DestEP,
		DestEP:   msg.APS.SourceEP,
		Options:  ezsp.APSOptionRetry,
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := conn.SendUnicast(sendCtx, msg.Sender, reply, 0, payload); err != nil {
		fmt.Fprintf(os.Stderr, "gzb: answering time read from 0x%04X: %v\n", msg.Sender, err)
		return false
	}
	return true
}
