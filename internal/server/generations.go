package server

import (
	"github.com/gin-gonic/gin"
)

// generations implements POST /v1/images/generations.
func (s *Server) generations(c *gin.Context) (*ImagesResponse, error) {
	var req GenerationRequest
	raw, err := decodeJSON(c, maxJSONBody, &req)
	if err != nil {
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
	saveRequest(wd, jsonRecord(raw))
	return s.execute(ctx, wd, job)
}
