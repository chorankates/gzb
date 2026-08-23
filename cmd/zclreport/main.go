// Command zclreport is a throwaway repair tool: it reads the temperature
// reporting configuration off an untouched sensor and writes the same one to
// the sensor whose configuration was cleared, then reads it back to confirm.
// Delete it once the repair is done.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/chorankates/gzb/zigbee"
)

const (
	control = 0xB306 // sunroom thermo, never configured by gzb
	broken  = 0xCF83 // living room thermo, whose configuration was cleared
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	c, err := zigbee.Open(ctx, zigbee.Options{Path: "/dev/ttyUSB0"})
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer c.Close()

	readings, errs := c.Readings(ctx)
	go func() {
		for r := range readings {
			fmt.Printf("%s  REPORT %s %.2f %s from 0x%04X\n",
				stamp(), r.Capability, r.Value, r.Unit, r.NodeID)
		}
	}()
	go func() {
		for range errs {
		}
	}()

	reference, ok := read(ctx, c, control, "REFERENCE", 60)
	if !ok {
		fmt.Println("### could not read the control sensor; nothing was changed")
		return
	}
	if !reference.Reporting {
		fmt.Println("### the control sensor reports nothing either; nothing to restore")
		return
	}

	restore := zigbee.ReportConfig{
		ID:   reference.ID,
		Type: reference.Type,
		Min:  reference.Min,
		Max:  reference.Max,
	}
	if reference.Change != nil {
		restore.Change = *reference.Change
	}
	fmt.Printf("%s  restoring min=%s max=%s change=%v type=%s\n",
		stamp(), restore.Min, restore.Max, restore.Change, restore.Type)

	if !apply(ctx, c, broken, restore, 90) {
		fmt.Println("### WARNING: could not write the configuration back")
		return
	}
	after, ok := read(ctx, c, broken, "VERIFY", 90)
	if !ok {
		fmt.Println("### wrote the configuration but could not read it back")
		return
	}
	fmt.Printf("### after repair: reporting=%v min=%s max=%s change=%v\n",
		after.Reporting, after.Min, after.Max, deref(after.Change))
}

func target(node uint16) zigbee.Target {
	return zigbee.Target{Node: node, Endpoint: 1, Cluster: 0x0402}
}

func read(ctx context.Context, c *zigbee.Coordinator, node uint16, label string, tries int) (zigbee.ReportingStatus, bool) {
	for i := 1; i <= tries; i++ {
		tryCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		holding, err := c.ReportingConfiguration(tryCtx, target(node), []uint16{0x0000})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return zigbee.ReportingStatus{}, false
			}
			continue
		}
		if len(holding) == 0 {
			return zigbee.ReportingStatus{}, false
		}
		fmt.Printf("%s  %s 0x%04X on attempt %d: reporting=%v min=%s max=%s change=%v type=%s\n",
			stamp(), label, node, i, holding[0].Reporting, holding[0].Min, holding[0].Max,
			deref(holding[0].Change), holding[0].Type)
		return holding[0], true
	}
	return zigbee.ReportingStatus{}, false
}

func apply(ctx context.Context, c *zigbee.Coordinator, node uint16, cfg zigbee.ReportConfig, tries int) bool {
	for i := 1; i <= tries; i++ {
		tryCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		results, err := c.ConfigureReporting(tryCtx, target(node), []zigbee.ReportConfig{cfg})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			continue
		}
		fmt.Printf("%s  RESTORE 0x%04X on attempt %d: ok=%v status=%q\n",
			stamp(), node, i, results[0].OK, results[0].Status)
		return results[0].OK
	}
	return false
}

func deref(v *uint64) any {
	if v == nil {
		return nil
	}
	return *v
}

func stamp() string { return time.Now().Format("15:04:05") }
