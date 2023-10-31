/***************************************************************
 *
 * Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package origin_ui

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pelicanplatform/pelican/director"
	"github.com/pelicanplatform/pelican/metrics"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type (
	DirectorTest struct {
		Status    string `json:"status"`
		Message   string `json:"message"`
		Timestamp string `json:"timestamp"`
	}
)

var (
	// Mutex for safe concurrent access to the timer
	timerMutex sync.Mutex
	// Timer for tracking timeout
	directorTimeoutTimer *time.Timer
	// Duration to wait before timeout
	// TODO: Do we want to make this a configurable value?
	directorTimeoutDuration = 30 * time.Second
	exitSignals             = make(chan os.Signal, 1)
	exitLoop                = make(chan struct{})
)

func apiAuthHandler(ctx *gin.Context) {
	authHeader := ctx.Request.Header.Get("Authorization")

	// Check if the Authorization header was provided
	if authHeader == "" {
		// Use AbortWithStatusJSON to stop invoking the next chain
		ctx.AbortWithStatusJSON(401, gin.H{"error": "Authorization header is missing"})
		return
	}

	// Check if the Authorization type is Bearer
	if !strings.HasPrefix(authHeader, "Bearer ") {
		ctx.AbortWithStatusJSON(401, gin.H{"error": "Authorization header is not Bearer type"})
		return
	}

	// Extract the token from the Authorization header
	token := strings.TrimPrefix(authHeader, "Bearer ")
	valid, err := director.VerifyDirectorTestReportToken(token)

	if err != nil {
		log.Warningln(fmt.Sprintf("Error when verifying Bearer token: %s", err))
		ctx.AbortWithStatusJSON(401, gin.H{"error": fmt.Sprintf("Error when verifying Bearer token: %s", err)})
		return
	}

	if !valid {
		log.Warningln("Can't validate Bearer token")
		ctx.AbortWithStatusJSON(401, gin.H{"error": "Can't validate Bearer token"})
		return
	}
	ctx.Next()
}

// Reset the timer safely
func resetDirectorTimeoutTimer() {
	timerMutex.Lock()
	defer timerMutex.Unlock()

	if directorTimeoutTimer == nil {
		directorTimeoutTimer = time.NewTimer(directorTimeoutDuration)
		go func() {
			for {
				select {
				case <-directorTimeoutTimer.C:
					// Timer fired because no message was received in time.
					log.Warningln("No director test report received within the time limit")
					if err := metrics.SetComponentHealthStatus("director", "critical", "No director test report received within the time limit"); err != nil {
						log.Errorln("Failed to update director component health status:", err)
					}
					// Reset the timer for the next period.
					timerMutex.Lock()
					directorTimeoutTimer.Reset(directorTimeoutDuration)
					timerMutex.Unlock()
				case <-exitLoop:
					log.Infoln("Gracefully terminating the director-health test timeout loop...")
					return
				}
			}
		}()
	} else {
		if !directorTimeoutTimer.Stop() {
			<-directorTimeoutTimer.C
		}
		directorTimeoutTimer.Reset(directorTimeoutDuration)
	}
}

// Director will periodiclly upload/download files to/from all connected
// origins and test the health status of origins. It will send a request
// reporting such status to this endpoint, and we will update origin internal
// health status metric to reflect the director connection status.
func directorTestResponse(ctx *gin.Context) {
	dt := DirectorTest{}
	if err := ctx.ShouldBind(&dt); err != nil {
		log.Errorf("Invalid director test response")
		ctx.JSON(400, gin.H{"error": "Invalid director test response"})
		return
	}
	// We will let the timer go timeout if director didn't send a valid json request
	resetDirectorTimeoutTimer()
	if dt.Status == "ok" {
		if err := metrics.SetComponentHealthStatus("director", "ok", fmt.Sprintf("Director timestamp: %s", dt.Timestamp)); err != nil {
			log.Errorln("Failed to update director component health status:", err)
			ctx.JSON(500, gin.H{"error": fmt.Sprintf("Failed to update director component health status: %s", err)})
			return
		}
	} else if dt.Status == "error" {
		if err := metrics.SetComponentHealthStatus("director", "critical", dt.Message); err != nil {
			log.Errorln("Failed to update director component health status:", err)
			ctx.JSON(500, gin.H{"error": fmt.Sprintf("Failed to update director component health status: %s", err)})
			return
		}
	} else {
		log.Errorf("Invalid director test response, status: %s", dt.Status)
		ctx.JSON(400, gin.H{"error": fmt.Sprintf("Invalid director test response status: %s", dt.Status)})
	}
}

func ConfigureOriginAPI(router *gin.Engine) error {
	if router == nil {
		return errors.New("Origin configuration passed a nil pointer")
	}
	if err := metrics.SetComponentHealthStatus("director", "warning", "Initializing origin, unknown status for director"); err != nil {
		return err
	}
	// start the timer for the director test report timeout
	resetDirectorTimeoutTimer()

	// When program exits
	signal.Notify(exitSignals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		// Gracefully stop the timer at the exit of the program
		<-exitSignals
		timerMutex.Lock()
		defer timerMutex.Unlock()
		log.Infoln("Gracefully stopping the director-health test timeout timer...")
		// Terminate the infinite loop to reset the timer
		close(exitLoop)
		if directorTimeoutTimer != nil {
			directorTimeoutTimer.Stop()
			directorTimeoutTimer = nil
		}
	}()

	group := router.Group("/api/v1.0/origin-api")
	group.Use(apiAuthHandler)
	group.POST("/directorTest", directorTestResponse)

	return nil
}
