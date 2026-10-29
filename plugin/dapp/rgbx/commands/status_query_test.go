package commands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The single-query withdrawal/config commands must be reachable from the rgbx root and keep
// their documented flags.
func Test_statusQueryCommandsAreRegistered(t *testing.T) {
	root := Cmd()

	final := findSubCmd(t, root, "get-withdraw-final-state")
	require.NoError(t, final.MarkFlagRequired("tx-hash"))

	status := findSubCmd(t, root, "get-withdraw-status")
	require.NoError(t, status.MarkFlagRequired("tx-hash"))

	effective := findSubCmd(t, root, "get-effective-bridge-config")
	flag := effective.Flags().Lookup("asset-symbol")
	require.NotNil(t, flag)
	require.Empty(t, flag.DefValue, "an empty symbol addresses the global config")
}
