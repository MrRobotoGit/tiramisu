package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"

	sets "tiramisu/internal/gostorm/settings"
	"tiramisu/internal/gostorm/torr"
)

// Action: get, set, def
type setsReqJS struct {
	requestI
	Sets *sets.BTSets `json:"sets,omitempty"`
}

// settings godoc
//
//	@Summary		Get / Set server settings
//	@Description	Allow to get or set server settings.
//
//	@Tags			API
//
//	@Param			request	body	setsReqJS	true	"Settings request. Available params for action: get, set, def"
//
//	@Accept			json
//	@Produce		json
//	@Success		200	{object}	sets.BTSets	"Settings JSON or nothing. Depends on what action has been asked."
//	@Router			/settings [post]
func settings(c *gin.Context) {
	var req setsReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	if req.Action == "get" {
		c.JSON(200, sets.BTsets)
		return
	} else if req.Action == "set" {
		torr.SaveSettings(req.Sets, origin(c, ""))
		c.Status(200)
		return
	} else if req.Action == "def" {
		torr.SaveDefSettings(origin(c, "defaults"))
		c.Status(200)
		return
	}
	c.AbortWithError(http.StatusBadRequest, errors.New("action is empty"))
}

// origin is who saved, from the :9080 proxy headers, for the Engine history.
func origin(c *gin.Context, note string) torr.Origin {
	source, actor := sets.ActorFromRequest(c.Request.RemoteAddr, c.GetHeader(sets.HeaderSource), c.GetHeader(sets.HeaderActor))
	return torr.Origin{Source: source, Actor: actor, Note: note}
}
