package commands

func extendCommand() Definition {
	return Definition{
		Name:        "extend",
		Description: "Run a message with the extend_turn_iteration tool enabled",
		Usage:       "/extend <message>",
	}
}