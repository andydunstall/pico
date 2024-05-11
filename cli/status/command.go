package status

import "github.com/spf13/cobra"

func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "inspect server status",
		Long: `Inspect server status.

Each Pico server exposes a status API to inspect the state of the node, this
can be used to answer questions such as:
* What upstream listeners are attached to each node?
* What cluster state does this node know?
* What is the gossip state of each known node?

See 'status --help' for the availale commands.

Examples:
  # Inspect the known nodes in the cluster.
  pico status cluster nodes

  # Inspect the upstream listeners connected to this node.
  pico status proxy endpoints

  # Inspect the status of server 10.26.104.56:8002.
  pico status proxy endpoints --server 10.26.104.56:8002
`,
	}

	cmd.AddCommand(newProxyCommand())
	cmd.AddCommand(newClusterCommand())
	cmd.AddCommand(newGossipCommand())

	return cmd
}
