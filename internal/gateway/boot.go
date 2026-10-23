package gateway

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/difof/mcs-bootloader/internal/control"
)

// Only the retry disconnect text is templated. Gameplay packets and native
// status metadata remain untouched. The current attempt's timestamp, rather
// than connection arrival, anchors every retry to the same countdown.
func bootMessage(template string, snapshot control.Snapshot, now time.Time) string {
	elapsed := time.Duration(0)
	elapsedText := "unknown"
	if !snapshot.BootStartedAt.IsZero() {
		elapsed = max(now.Sub(snapshot.BootStartedAt), 0)
		elapsedText = displayBootDuration(elapsed)
	}
	eta := "Startup time is not known yet."
	estimate := snapshot.BootEstimateSeconds
	if !snapshot.BootStartedAt.IsZero() && estimate > 0 && !math.IsNaN(estimate) && !math.IsInf(estimate, 0) && estimate <= (24*time.Hour).Seconds() {
		remaining := time.Duration(estimate*float64(time.Second)) - elapsed
		if remaining > 0 {
			// Round remaining time upwards so the text never promises readiness
			// at 0s while the backend is still starting.
			remaining = ((remaining + time.Second - 1) / time.Second) * time.Second
			eta = "About " + displayBootDuration(remaining) + " remaining."
		} else {
			eta = "Taking longer than estimated; still starting."
		}
	}
	message := strings.NewReplacer("{eta}", eta, "{elapsed}", elapsedText).Replace(template)
	// Repeated placeholders may expand beyond the configured text limit. Keep
	// the rendered UTF-8 message bounded and within the login packet limit.
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func displayBootDuration(value time.Duration) string {
	return max(value, 0).Truncate(time.Second).String()
}
