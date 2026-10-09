package server

import (
	"github.com/gin-gonic/gin"
)

// generations implements POST /v1/images/generations.
func (s *Server) generations(c *gin.Context) (*ImagesResponse, error) {
	var req GenerationRequest
	if err := decodeJSON(c, maxJSONBody, &req); err != nil {
		return nil, err
	}
	job, err := s.buildJob(req.Prompt, req.N, req.Background, req.Size)
	if err != nil {
		return nil, err
	}

	wd, err := s.runner.NewWorkdir(c.GetString(requestIDKey))
	if err != nil {
		return nil, err
	}
	ctx := c.Request.Context()
	defer s.runner.Cleanup(ctx, wd)
	return s.execute(ctx, wd, job)
}
