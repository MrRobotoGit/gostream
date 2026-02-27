package utils

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MrRobotoGit/gostream/internal/gostorm/log"

	"github.com/MrRobotoGit/gostream/internal/gostorm/settings"

	"github.com/anacrolix/torrent/iplist"
)

func ReadBlockedIP() (ranger iplist.Ranger, err error) {
	// 1. Try executable directory first (new auto-update location)
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		buf, err := os.ReadFile(filepath.Join(exeDir, "blocklist"))
		if err == nil {
			log.TLogln("Read block list from binary directory...")
			return parseBlockList(buf)
		}
	}

	// 2. Fallback to settings.Path
	buf, err := os.ReadFile(filepath.Join(settings.Path, "blocklist"))
	if err != nil {
		return nil, err
	}
	log.TLogln("Read block list from settings directory...")
	return parseBlockList(buf)
}

func parseBlockList(buf []byte) (iplist.Ranger, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	var ranges []iplist.Range
	lineCount := 0
	errorCount := 0
	for scanner.Scan() {
		lineCount++
		r, ok, err := iplist.ParseBlocklistP2PLine(scanner.Bytes())
		if err != nil {
			errorCount++
			continue // Skip malformed lines
		}
		if ok {
			ranges = append(ranges, r)
		}
	}
	
	if err := scanner.Err(); err != nil {
		log.TLogln("Scanner error during blocklist parse:", err)
	}
	
	if len(ranges) > 0 {
		log.TLogln(fmt.Sprintf("Readed ranges: %d (Total lines: %d, Errors: %d)", len(ranges), lineCount, errorCount))
		return iplist.New(ranges), nil
	}
	log.TLogln(fmt.Sprintf("No ranges loaded from blocklist! (Lines read: %d, Errors: %d)", lineCount, errorCount))
	return nil, scanner.Err()
}
