package zigbee

import (
	"context"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/zdo"
)

// A device that joined while nothing was listening answers the broadcast
// itself, and ends up in the registry under its network address.
func TestFindRecordsTheAnsweringDevice(t *testing.T) {
	fake := newFakeConnection()
	fake.responder = func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		if dest != ezsp.BroadcastRxOnWhenIdle || aps.Cluster != zdo.ClusterNetworkAddress {
			t.Errorf("sent to 0x%04X cluster 0x%04X, want a broadcast NWK_addr_req", dest, aps.Cluster)
		}
		// seq, status, ieee (wire order), nwk address.
		reply := append([]byte{payload[0], 0x00}, payload[1:9]...)
		reply = append(reply, 0xA4, 0x68)
		return ezsp.IncomingMessage{
			Sender:  0x68A4,
			APS:     ezsp.APSFrame{Profile: ezsp.ProfileZDO, Cluster: zdo.ResponseCluster(zdo.ClusterNetworkAddress)},
			Payload: reply,
		}, true
	}
	c := &Coordinator{conn: fake, db: emptyStore(t)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := c.Find(ctx, "70:D0:7E:FF:FE:A3:A1:92")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if d.IEEE != "70:D0:7E:FF:FE:A3:A1:92" || d.NodeID != 0x68A4 {
		t.Fatalf("found %s at 0x%04X, want 70:D0:7E:FF:FE:A3:A1:92 at 0x68A4", d.IEEE, d.NodeID)
	}
	if _, err := c.Device("0x68A4"); err != nil {
		t.Fatalf("not in the registry afterwards: %v", err)
	}
}

func TestFindRejectsABadAddress(t *testing.T) {
	c := &Coordinator{conn: newFakeConnection(), db: emptyStore(t)}
	if _, err := c.Find(context.Background(), "not-an-address"); err == nil {
		t.Fatal("Find accepted a malformed address")
	}
}
