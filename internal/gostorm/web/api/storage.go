package api

import (
	"net/http"

	sets "github.com/MrRobotoGit/gostream/internal/gostorm/settings"

	"github.com/gin-gonic/gin"
)

func GetStorageSettings(c *gin.Context) {
	prefs := sets.GetStoragePreferences()
	c.JSON(http.StatusOK, prefs)
}

func UpdateStorageSettings(c *gin.Context) {
	if sets.ReadOnly {
		c.JSON(http.StatusForbidden, gin.H{"error": "Read-only mode"})
		return
	}

	var prefs map[string]interface{}

	// Check Content-Type to handle both JSON and form data
	contentType := c.GetHeader("Content-Type")

	if contentType == "application/x-www-form-urlencoded" {
		// Handle form data
		settings := c.PostForm("settings")

		prefs = make(map[string]interface{})
		if settings != "" {
			prefs["settings"] = settings
		}
	} else {
		// Handle JSON (default)
		if err := c.ShouldBindJSON(&prefs); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	if settingsPref, ok := prefs["settings"].(string); ok && settingsPref != "" {
		if settingsPref != "json" && settingsPref != "bbolt" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid settings storage value"})
			return
		}
	}

	// Check if we have at least one value to update
	if len(prefs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No preferences provided"})
		return
	}

	if err := sets.SetStoragePreferences(prefs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
