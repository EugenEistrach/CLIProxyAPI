package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetRoutingDecisions returns the reset-aware selector's recent picks, newest first, each with
// every candidate's weekly and five-hour usage and the reason it was or wasn't chosen.
func (h *Handler) GetRoutingDecisions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"decisions": coreauth.RecentResetAwareDecisions()})
}
