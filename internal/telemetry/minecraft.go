package telemetry

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	formatting        = regexp.MustCompile(`(?i)§[0-9a-fk-or]`)
	listPattern       = regexp.MustCompile(`^There are ([0-9]+) of a max of ([0-9]+) players online:\s*(.*)$`)
	playerPattern     = regexp.MustCompile(`^([A-Za-z0-9_]{1,16}) \(([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\)$`)
	namePattern       = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
	ratePattern       = regexp.MustCompile(`Target tick rate: ([0-9]+(?:\.[0-9]+)?) per second\.`)
	meanPattern       = regexp.MustCompile(`Average time per tick: ([0-9]+(?:\.[0-9]+)?)ms \(Target: ([0-9]+(?:\.[0-9]+)?)ms\)`)
	percentilePattern = regexp.MustCompile(`Percentiles: P50: ([0-9]+(?:\.[0-9]+)?)ms P95: ([0-9]+(?:\.[0-9]+)?)ms P99: ([0-9]+(?:\.[0-9]+)?)ms, sample: ([0-9]+)`)
	dimensionPattern  = regexp.MustCompile(`^[a-z0-9_.-]+:[a-z0-9_./-]+$`)
)

type player struct{ Name, UUID string }

func parsePlayers(text string, details bool) (int, int, []player, error) {
	text = strings.TrimSpace(formatting.ReplaceAllString(text, ""))
	parts := listPattern.FindStringSubmatch(text)
	if len(parts) != 4 {
		return 0, 0, nil, errors.New("unrecognized native player list")
	}
	count, e1 := strconv.Atoi(parts[1])
	capacity, e2 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || count < 0 || capacity < 0 {
		return 0, 0, nil, errors.New("invalid native player count")
	}
	var players []player
	if count == 0 {
		if strings.TrimSpace(parts[3]) != "" {
			return 0, 0, nil, errors.New("contradictory native empty player list")
		}
		return count, capacity, players, nil
	}
	names := strings.Split(parts[3], ", ")
	if len(names) != count {
		return 0, 0, nil, errors.New("native player list count mismatch")
	}
	seen := make(map[string]bool)
	for _, item := range names {
		p := player{Name: item}
		if details {
			match := playerPattern.FindStringSubmatch(item)
			if len(match) != 3 {
				return 0, 0, nil, errors.New("unrecognized native player UUID list")
			}
			p.Name, p.UUID = match[1], strings.ToLower(match[2])
		} else if !namePattern.MatchString(item) {
			return 0, 0, nil, errors.New("unrecognized native player name")
		}
		if seen[strings.ToLower(p.Name)] {
			return 0, 0, nil, errors.New("duplicate native player name")
		}
		seen[strings.ToLower(p.Name)] = true
		players = append(players, p)
	}
	return count, capacity, players, nil
}

func playerPoints(text string, details bool) ([]point, error) {
	count, capacity, players, err := parsePlayers(text, details)
	if err != nil {
		return nil, err
	}
	return playerListPoints(count, capacity, players, details), nil
}

func playerListPoints(count, capacity int, players []player, details bool) []point {
	points := []point{gauge("mcs_minecraft_players_online", "Online players reported by native RCON list.", float64(count)), gauge("mcs_minecraft_players_capacity", "Player capacity reported by native RCON list.", float64(capacity))}
	if details {
		for _, p := range players {
			points = append(points, gauge("mcs_minecraft_player_online", "Online player from native list uuids; enabled only with player details.", 1, "name", p.Name, "uuid", p.UUID))
		}
	}
	return points
}

func tickPoints(text string) ([]point, error) {
	text = formatting.ReplaceAllString(text, "")
	rate, mean := ratePattern.FindStringSubmatch(text), meanPattern.FindStringSubmatch(text)
	if len(rate) != 2 || len(mean) != 3 {
		return nil, errors.New("unrecognized native tick query")
	}
	r, e1 := strconv.ParseFloat(rate[1], 64)
	avg, e2 := strconv.ParseFloat(mean[1], 64)
	target, e3 := strconv.ParseFloat(mean[2], 64)
	if e1 != nil || e2 != nil || e3 != nil || !finite(r) || !finite(avg) || !finite(target) || r <= 0 || avg < 0 || target <= 0 {
		return nil, errors.New("invalid native tick timing")
	}
	points := []point{gauge("mcs_minecraft_tick_target_rate_hertz", "Configured target tick rate, not a measured TPS estimate.", r), gauge("mcs_minecraft_tick_duration_seconds", "Mean tick execution time reported by native tick query.", avg/1000), gauge("mcs_minecraft_tick_target_duration_seconds", "Configured target tick duration.", target/1000)}
	if strings.Contains(text, "The game is frozen") || strings.Contains(text, "The game is running normally") || strings.Contains(text, "The game is sprinting") {
		points = append(points, gauge("mcs_minecraft_tick_frozen", "Native tick query reports the game as frozen.", boolean(strings.Contains(text, "The game is frozen"))), gauge("mcs_minecraft_tick_sprinting", "Native tick query reports the game as sprinting.", boolean(strings.Contains(text, "The game is sprinting"))))
	}
	percentiles := percentilePattern.FindStringSubmatch(text)
	if len(percentiles) == 5 {
		for i, q := range []string{"0.5", "0.95", "0.99"} {
			v, err := strconv.ParseFloat(percentiles[i+1], 64)
			if err != nil || !finite(v) {
				return nil, errors.New("invalid tick percentile")
			}
			points = append(points, gauge("mcs_minecraft_tick_duration_quantile_seconds", "Native tick query sample percentiles.", v/1000, "quantile", q))
		}
		n, err := strconv.ParseUint(percentiles[4], 10, 64)
		if err != nil {
			return nil, errors.New("invalid tick sample size")
		}
		points = append(points, gauge("mcs_minecraft_tick_samples", "Ticks in the native tick query sample.", float64(n)))
	}
	return points, nil
}

var playerFields = [...]struct{ Path, Metric, Help string }{
	{"Health", "mcs_minecraft_player_health", "Player health points from native data get."},
	{"foodLevel", "mcs_minecraft_player_food_level", "Player food level from native data get."},
	{"foodSaturationLevel", "mcs_minecraft_player_food_saturation", "Player food saturation from native data get."},
	{"XpLevel", "mcs_minecraft_player_experience_level", "Player experience level from native data get."},
	{"XpTotal", "mcs_minecraft_player_experience_total", "Player current stored total experience from native data get; may decrease."},
	{"playerGameType", "mcs_minecraft_player_game_mode", "Native player game mode ID: survival0 creative1 adventure2 spectator3."},
	{"Pos", "mcs_minecraft_player_position", "Player position in blocks from native data get."},
	{"Dimension", "mcs_minecraft_player_dimension_info", "Player dimension identifier from native data get."},
}

func playerFieldPoints(p player, field int, text string) ([]point, error) {
	if !namePattern.MatchString(p.Name) || field < 0 || field >= len(playerFields) {
		return nil, errors.New("invalid player detail request")
	}
	text = strings.TrimSpace(formatting.ReplaceAllString(text, ""))
	prefix := p.Name + " has the following entity data: "
	if !strings.HasPrefix(text, prefix) {
		return nil, errors.New("unrecognized native entity data")
	}
	value := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	f := playerFields[field]
	labels := []string{"name", p.Name, "uuid", p.UUID}
	if f.Path == "Dimension" {
		d, err := strconv.Unquote(value)
		if err != nil || len(d) > 256 || !dimensionPattern.MatchString(d) {
			return nil, errors.New("invalid player dimension")
		}
		return []point{gauge(f.Metric, f.Help, 1, append(labels, "dimension", d)...)}, nil
	}
	if f.Path == "Pos" {
		if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
			return nil, errors.New("invalid player position")
		}
		parts := strings.Split(value[1:len(value)-1], ",")
		if len(parts) != 3 {
			return nil, errors.New("invalid player position length")
		}
		var points []point
		for i, axis := range []string{"x", "y", "z"} {
			v, err := nbtNumber(parts[i])
			if err != nil {
				return nil, err
			}
			points = append(points, gauge(f.Metric, f.Help, v, append(append([]string{}, labels...), "axis", axis)...))
		}
		return points, nil
	}
	v, err := nbtNumber(value)
	if err != nil {
		return nil, err
	}
	return []point{gauge(f.Metric, f.Help, v, labels...)}, nil
}

func nbtNumber(text string) (float64, error) {
	text = strings.TrimSpace(text)
	if len(text) > 0 && strings.ContainsRune("bBsSlLfFdD", rune(text[len(text)-1])) {
		text = text[:len(text)-1]
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || !finite(v) {
		return 0, errors.New("invalid numeric entity data")
	}
	return v, nil
}
func finite(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }
