package utils

import (
	"net/http"
	"strings"

	"github.com/MrRobotoGit/gostream/internal/gostorm/log"
)

func CheckImgUrl(link string) bool {
	if link == "" {
		return false
	}
	// Try HEAD first to save bandwidth
	resp, err := http.Head(link)
	if err != nil || resp.StatusCode != http.StatusOK {
		// Fallback to GET if HEAD fails (some servers block HEAD)
		resp, err = http.Get(link)
		if err != nil {
			log.TLogln("Error check image:", err)
			return false
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	ctype := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ctype, "image/")
}
