package rcon

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Native English list formats: 1.7-1.12 uses N/M, 1.13-1.15 uses "of a max M",
// and 1.16+ uses "of a max of M". Source: PrismarineJS/minecraft-data at
// 1df9432c07c81b47326e4fb1e06d3c8d723bf4aa, data/pc/*/language.json.
// Unknown/localized/modded formats remain errors, never an inferred empty list.
var playerList = regexp.MustCompile(`^There are ([0-9]+)(?:/| of a max (?:of )?)([0-9]+) players online:\s*(.*)$`)
var formatting = regexp.MustCompile(`(?i)§[0-9a-fk-or]`)

func parsePlayers(response string) (int, error) {
	response = strings.TrimSpace(formatting.ReplaceAllString(response, ""))
	parts := playerList.FindStringSubmatch(response)
	if len(parts) != 4 {
		return 0, errors.New("unrecognized RCON player-list response")
	}
	count, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, errors.New("invalid RCON player count")
	}
	max, err := strconv.Atoi(parts[2])
	if err != nil || max < 0 || count < 0 {
		return 0, errors.New("invalid RCON player capacity")
	}
	// A supposedly empty list with any unexpected trailing text is ambiguous.
	if count == 0 && strings.TrimSpace(parts[3]) != "" {
		return 0, errors.New("contradictory RCON empty-player response")
	}
	if count > 0 && strings.TrimSpace(parts[3]) == "" {
		return 0, errors.New("RCON player names missing from nonempty list")
	}
	return count, nil
}

func (c *Client) Players(ctx context.Context) (int, error) {
	response, err := c.Command(ctx, "list")
	if err != nil {
		return 0, err
	}
	return parsePlayers(response)
}

func isStopReply(response string) bool {
	response = strings.TrimSpace(formatting.ReplaceAllString(response, ""))
	return response == "Stopping the server" || response == "Stopping server"
}

func (c *Client) Stop(ctx context.Context) error {
	response, err := c.Command(ctx, "stop")
	if err != nil {
		return err
	}
	if !isStopReply(response) {
		return errors.New("RCON stop was not acknowledged")
	}
	return nil
}
