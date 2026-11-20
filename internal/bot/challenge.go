package bot

import (
	"slices"

	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
)

// EvaluateChallenge decides whether an incoming challenge is accepted. When it
// is not, reason is a Lichess decline reason key.
//
// Real time games are judged on their estimated duration, initial time plus
// 40 increments (the formula Lichess uses for speed categories), because the
// LLM needs a few seconds per move.
func EvaluateChallenge(c config.Challenges, ch lichess.Challenge, busy bool) (accept bool, reason string) {
	switch {
	case busy:
		return false, "later"
	case !slices.Contains(c.Variants, ch.Variant.Key):
		if len(c.Variants) == 1 && c.Variants[0] == "standard" {
			return false, "standard"
		}
		return false, "variant"
	case ch.Rated && !c.AllowRated:
		return false, "casual"
	case !ch.Rated && !c.AllowCasual:
		return false, "rated"
	case ch.Challenger.Title == "BOT" && !c.AllowBots:
		return false, "noBot"
	}

	switch ch.TimeControl.Type {
	case "clock":
		estimated := ch.TimeControl.Limit + 40*ch.TimeControl.Increment
		if estimated < c.MinInitialSeconds || ch.TimeControl.Increment < c.MinIncrementSeconds {
			return false, "tooFast"
		}
	default: // correspondence, unlimited
		if !c.AllowCorrespondence {
			return false, "tooSlow"
		}
	}
	return true, ""
}
