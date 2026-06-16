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

func ErrorWithCode(c *gin.Context, code int, errorCode string, err string) {
	c.AbortWithStatusJSON(code, ResponseStruct{
		Code:      code,
		ErrorCode: errorCode,
		Message:   err,
	})
}
