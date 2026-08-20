package resp

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type ResponseStruct struct {
	Code      int         `json:"code" example:"200"`
	ErrorCode string      `json:"error_code,omitempty"`
	Message   string      `json:"message" example:"success"`
	Data      interface{} `json:"data,omitempty"`
}

func Success(c *gin.Context, data any) {
	c.JSON(http.StatusOK, ResponseStruct{
		Code:    http.StatusOK,
		Message: "success",
		Data:    data,
	})
}

func Error(c *gin.Context, code int, err string) {
	ErrorWithCode(c, code, "", err)
}

// OpenAIError writes an OpenAI-standard error envelope
// {"error":{"message":..,"type":..,"code":..}} with the given HTTP status code.
//
// Why this exists: octopus's own ResponseStruct {code,error_code,message} is octopus-internal
// and NOT what OpenAI / cursor / Anthropic SDK clients parse. Cursor's responses parser in
// particular surfaces "OpenAI Responses API failed: unknown error" when it receives a non-OpenAI
// error body on /v1/responses (it expects {"error":{"message":..,"type":..,"code":..}}).
// Mirror new-api's types.NewAPIError contract: ALWAYS emit OpenAI shape to OpenAI-protocol
// clients. Use this on the /v1/responses inbound path pre-stream error branches (model not
// supported, route selection, parseRequest validation, last-resort after all channels fail
// pre-commit). Post-stream branches use writeResponsesFailedSSE / writeChatErrorSSE which
// already emit the correct shape on the committed SSE stream.
func OpenAIError(c *gin.Context, code int, errType string, errCode string, message string) {
	if errType == "" {
		errType = "invalid_request_error"
	}
	c.AbortWithStatusJSON(code, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
			"code":    errCode,
		},
	})
}

func ErrorWithCode(c *gin.Context, code int, errorCode string, err string) {
	c.AbortWithStatusJSON(code, ResponseStruct{
		Code:      code,
		ErrorCode: errorCode,
		Message:   err,
	})
}
