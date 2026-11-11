package collector

import (
	"fmt"

	"github.com/robfig/cron/v3"
)

// cronParser accepts the 5-field standard cron format plus descriptors
// such as @daily, @weekly and @every 1h.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// ValidateCronSpec reports whether spec is a usable schedule expression.
func ValidateCronSpec(spec string) error {
	if _, err := cronParser.Parse(spec); err != nil {
		return fmt.Errorf("invalid cron spec %q: %w", spec, err)
	}
	return nil
}
