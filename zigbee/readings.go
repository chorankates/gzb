package zigbee

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/internal/zcl"
)

// Readings starts the coordinator's protocol loop. The readings and errors
// channels close when ctx is cancelled, the coordinator closes, or startup
// validation fails. Malformed device frames are reported on the error channel
// without stopping later readings.
//
// Only one Readings call may be active on a Coordinator. This guarantees that
// application frames and coordinator service requests are handled once.
func (c *Coordinator) Readings(ctx context.Context) (<-chan Reading, <-chan error) {
	out := make(chan Reading, 128)
	errs := make(chan error, 16)

	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		close(out)
		errs <- ErrClosed
		close(errs)
		return out, errs
	case c.readingsActive:
		c.mu.Unlock()
		close(out)
		errs <- ErrReadingsActive
		close(errs)
		return out, errs
	}
	c.readingsActive = true
	done := make(chan struct{})
	c.readingsDone = done
	msgs, unsubscribe := c.conn.Subscribe(func(m ezsp.Message) bool {
		return m.Callback && m.ID == ezsp.FrameIncomingMessage
	}, 128)
	c.mu.Unlock()

	go func() {
		defer close(out)
		defer close(errs)
		defer unsubscribe()
		defer func() {
			c.mu.Lock()
			c.readingsActive = false
			c.readingsDone = nil
			close(done)
			c.mu.Unlock()
		}()

		state, err := c.conn.NetworkState(ctx)
		if err != nil {
			sendError(errs, fmt.Errorf("zigbee: reading network state: %w", err))
			return
		}
		if !state.Joined() {
			sendError(errs, fmt.Errorf("zigbee: no network on this adapter (%s)", state))
			return
		}

		for {
			select {
			case m, ok := <-msgs:
				if !ok {
					return
				}
				readings, err := c.handleIncoming(ctx, m)
				if err != nil {
					sendError(errs, err)
					continue
				}
				for _, reading := range readings {
					select {
					case out <- reading:
					case <-ctx.Done():
						c.save(errs)
						return
					}
				}
			case <-ctx.Done():
				c.save(errs)
				return
			}
		}
	}()

	return out, errs
}

func (c *Coordinator) handleIncoming(ctx context.Context, m ezsp.Message) ([]Reading, error) {
	msg, err := ezsp.DecodeIncomingMessage(m)
	if err != nil {
		return nil, err
	}
	// ZDO traffic is addressing rather than application data. Join handling
	// has its own decoder and the NCP answers other ZDO requests.
	if msg.APS.Profile == ezsp.ProfileZDO {
		return nil, nil
	}

	// Identity is captured as strings up front, so nothing below holds the
	// registry lock across network I/O or a caller's callback.
	now := time.Now()
	ieee, name := c.observeSender(msg.Sender, now)

	frame, err := zcl.Decode(msg.Payload)
	if err != nil {
		return nil, err
	}

	handled, err := serveTimeRead(ctx, c.conn, msg, frame)
	if err != nil {
		return nil, err
	}
	if handled {
		c.unhandled(eventFor(ieee, name, msg, now, "answered time read"))
		return nil, nil
	}

	attrs, err := frame.Attributes()
	if err != nil {
		c.unhandled(eventFor(ieee, name, msg, now, zcl.CommandName(frame.Command)))
		return nil, nil
	}

	quantities := make([]zcl.Reading, 0, len(attrs))
	for _, attr := range attrs {
		quantity, ok := zcl.Interpret(msg.APS.Cluster, attr)
		if !ok {
			description := fmt.Sprintf("%s = %v", zcl.AttributeName(msg.APS.Cluster, attr.ID), attr.Value)
			c.unhandled(eventFor(ieee, name, msg, now, description))
			continue
		}
		if !quantity.Current {
			// A device may volunteer a statistic it keeps about itself — the
			// warmest it has been, the range it can measure. That is worth
			// seeing and is not a reading, so it is reported as what it is
			// rather than filed under the quantity it resembles.
			c.unhandled(eventFor(ieee, name, msg, now, quantity.String()))
			continue
		}
		quantities = append(quantities, quantity)
	}
	if len(quantities) == 0 {
		return nil, nil
	}

	ieee, name = c.recordReadings(msg.Sender, quantities, now)
	readings := make([]Reading, 0, len(quantities))
	for _, quantity := range quantities {
		readings = append(readings, Reading{
			IEEE:       ieee,
			DeviceName: name,
			Capability: quantity.Name,
			Unit:       quantity.Unit,
			Value:      quantity.Value,
			At:         now,
			LQI:        msg.LQI,
			RSSI:       msg.RSSI,
			NodeID:     msg.Sender,
			Cluster:    zcl.ClusterName(msg.APS.Cluster),
		})
	}
	return readings, nil
}

// observeSender updates last-seen for a sender the registry knows and reports
// its identity — empty strings for a device with no settled identity yet.
func (c *Coordinator) observeSender(sender uint16, now time.Time) (ieee, name string) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	device, ok := c.db.ByNodeID(sender)
	if !ok {
		return "", ""
	}
	device.LastSeen = now
	return identityOf(device)
}

// recordReadings writes interpreted quantities to the registry and returns
// the identity the readings should carry.
func (c *Coordinator) recordReadings(sender uint16, quantities []zcl.Reading, now time.Time) (ieee, name string) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	device, ok := c.db.ByNodeID(sender)
	if !ok {
		// Traffic can arrive before a join callback supplies the stable
		// IEEE address. Preserve those readings under a temporary NodeID
		// record; Store.Observe promotes it when identity becomes known.
		device, _ = c.db.ObserveNodeID(sender, now)
	}
	for _, quantity := range quantities {
		device.Record(quantity.Name, quantity.Value, quantity.Unit, now)
	}
	return identityOf(device)
}

// identityOf is the identity a reading or event should report: nothing at all
// until the record carries a real IEEE address rather than a placeholder.
func identityOf(device *store.Device) (ieee, name string) {
	if !device.Identified() {
		return "", ""
	}
	return device.IEEE, device.Describe()
}

func eventFor(ieee, name string, msg ezsp.IncomingMessage, at time.Time, description string) Event {
	return Event{
		At:          at,
		IEEE:        ieee,
		DeviceName:  name,
		NodeID:      msg.Sender,
		Cluster:     zcl.ClusterName(msg.APS.Cluster),
		Description: description,
	}
}

func (c *Coordinator) unhandled(event Event) {
	if c.opts.OnUnhandled != nil {
		c.opts.OnUnhandled(event)
	}
}

func (c *Coordinator) save(errs chan<- error) {
	c.dbMu.Lock()
	err := c.db.Save()
	c.dbMu.Unlock()
	if err != nil {
		sendError(errs, err)
	}
}

func sendError(errs chan<- error, err error) {
	select {
	case errs <- err:
	default:
	}
}

// zigbeeEpoch is the origin of the ZCL UTCTime type.
var zigbeeEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	attrTime       uint16 = 0x0000
	attrTimeStatus uint16 = 0x0001
	attrTimeZone   uint16 = 0x0002
	attrDstStart   uint16 = 0x0003
	attrDstEnd     uint16 = 0x0004
	attrDstShift   uint16 = 0x0005
	attrLocalTime  uint16 = 0x0007

	timeStatusMaster uint8 = 0x03
)

func timeAttribute(id uint16, now time.Time) zcl.Record {
	utc := int64(now.UTC().Sub(zigbeeEpoch) / time.Second)
	_, offsetSeconds := now.Zone()

	switch id {
	case attrTime:
		return zcl.Record{ID: id, Type: zcl.TypeUTCTime, Value: clampZCLTime(utc)}
	case attrTimeStatus:
		return zcl.Record{ID: id, Type: zcl.TypeBitmap8, Value: uint64(timeStatusMaster)}
	case attrTimeZone:
		return zcl.Record{ID: id, Type: zcl.TypeInt32, Value: int64(offsetSeconds)}
	case attrDstStart, attrDstEnd:
		return zcl.Record{ID: id, Type: zcl.TypeUint32, Value: uint64(0)}
	case attrDstShift:
		return zcl.Record{ID: id, Type: zcl.TypeInt32, Value: int64(0)}
	case attrLocalTime:
		return zcl.Record{ID: id, Type: zcl.TypeUint32, Value: clampZCLTime(utc + int64(offsetSeconds))}
	default:
		return zcl.Record{ID: id, Status: zcl.StatusUnsupportedAttribute}
	}
}

func clampZCLTime(seconds int64) uint64 {
	if seconds < 0 {
		return 0
	}
	if seconds > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint64(seconds)
}

func serveTimeRead(ctx context.Context, conn connection, msg ezsp.IncomingMessage, frame zcl.Frame) (bool, error) {
	if msg.APS.Cluster != zcl.ClusterTime || frame.Command != zcl.CmdReadAttributes {
		return false, nil
	}
	ids, err := frame.ReadRequest()
	if err != nil {
		return false, err
	}

	now := time.Now()
	records := make([]zcl.Record, 0, len(ids))
	for _, id := range ids {
		records = append(records, timeAttribute(id, now))
	}
	payload, err := zcl.ReadAttributesResponse(frame.Sequence, records)
	if err != nil {
		return true, fmt.Errorf("zigbee: building time response: %w", err)
	}

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
		return true, fmt.Errorf("zigbee: answering time read from 0x%04X: %w", msg.Sender, err)
	}
	return true, nil
}
