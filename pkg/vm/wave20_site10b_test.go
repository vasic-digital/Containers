// SPDX-License-Identifier: Apache-2.0
package vm

// Wave-20 SITE-10b-ARGSWEEP permanent regression guard (§11.4.115 GREEN
// polarity): QEMU's `-drive` value is a comma-delimited key=value list, so an
// unescaped comma in the qcow2 path splits the option and misparses the drive.
// qemuDriveFile escapes commas as `,,` per QEMU's -drive grammar.
//
// Anti-tautology anchor: `strings.ReplaceAll(qcowPath, ",", ",,")` → `qcowPath`
// drops the escape → the comma case RED; restore → GREEN.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWave20_SITE10B_QemuDriveFileEscapesComma(t *testing.T) {
	require.Equal(t, "file=/cache/ab12cd34.qcow2,if=virtio",
		qemuDriveFile("/cache/ab12cd34.qcow2"),
		"a comma-free cache path must be unchanged")
	require.Equal(t, "file=/cache/a,,b.qcow2,if=virtio",
		qemuDriveFile("/cache/a,b.qcow2"),
		"a comma in the path must be escaped as ,, so it does not split the -drive option")
}
