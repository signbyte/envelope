package envelope

import (
	"fmt"
	"os"
	"strings"
)

// refuseEmptyFileSecret stops the start when a secret is named as a file
// (<name>_FILE) but gives no value: the file is missing or empty, for instance
// because the secrets agent has not written it yet. The reader treats that as
// "not set", and an unset database address silently selects the in-memory
// store; refusing here makes the service retry until the file is there.
func refuseEmptyFileSecret(name, value string) error {
	if os.Getenv(name+"_FILE") != "" && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s_FILE is set but gives no value: the file is missing or empty", name)
	}

	return nil
}
