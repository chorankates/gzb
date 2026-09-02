package zigbee

// Cluster-specific commands are how a hub tells a device to *do* something, as
// distinct from reading or writing its state. The difference is not a stylistic
// one: a light's CurrentHue is a read-only attribute, so the only way to change
// a colour is to send Move to Hue and Saturation and let the device update its
// own attribute. A hub that can only read and write attributes can watch a
// light, but it cannot operate one.

import (
	"context"
	"fmt"

	"github.com/chorankates/gzb/internal/zcl"
)

// SendCommand sends a cluster-specific command and waits for the device to say
// what it did with it.
//
// A cluster command has no response of its own — the device simply acts — so
// the answer is a ZCL Default Response, which is the only thing distinguishing
// a command that was refused from one that was obeyed. gzb therefore leaves
// the default response enabled and waits for it.
//
// The payload is the command's own arguments, already encoded. The zcl package
// builds the ones gzb models; a caller reaching past those is on its own with
// the specification, which is the point of leaving this generic.
func (c *Coordinator) SendCommand(ctx context.Context, t Target, cmd uint8, payload []byte) error {
	seq := c.nextSequence()
	frame, err := c.zclRequest(ctx, t, seq, zcl.CmdDefaultResponse, zcl.ClusterCommandRequest(seq, cmd, payload))
	if err != nil {
		return err
	}
	response, err := frame.DefaultResponse()
	if err != nil {
		return err
	}
	// The sequence already matched, so this only fires if a device answered
	// the wrong question — but a light that confirms a command nobody sent is
	// worth hearing about rather than reading as success.
	if response.Command != cmd {
		return fmt.Errorf("zigbee: %s answered about %s, not %s",
			t, zcl.ClusterCommandName(t.Cluster, response.Command), zcl.ClusterCommandName(t.Cluster, cmd))
	}
	if response.Status != zcl.StatusSuccess {
		return fmt.Errorf("zigbee: %s refused %s: %s",
			t, zcl.ClusterCommandName(t.Cluster, cmd), zcl.StatusName(response.Status))
	}
	return nil
}
