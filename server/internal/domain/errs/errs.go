// Package errs 定义全局业务错误码与错误类型。
//
// 业务码与 HTTP 状态码解耦：客户端判断业务结果只看 code，
// HTTP 状态码仅用于网关、监控和浏览器语义。
package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 是业务错误码。0 表示成功。
type Code int32

const (
	CodeSuccess Code = 0

	// 1xxx 请求与权限
	CodeInvalidParam    Code = 1000
	CodeUnauthorized    Code = 1001
	CodeForbidden       Code = 1002
	CodeNotFound        Code = 1003
	CodeTooManyRequests Code = 1004

	// 2xxx 资金与订单
	CodeInsufficientBalance Code = 2000
	CodeInvalidOrderState   Code = 2001

	// 3xxx 上游
	CodeUpstream Code = 3000

	// 5xxx 服务端
	CodeInternal Code = 5000
)

var messages = map[Code]string{
	CodeSuccess:             "success",
	CodeInvalidParam:        "参数校验失败",
	CodeUnauthorized:        "登录状态已失效，请重新登录",
	CodeForbidden:           "无权限执行该操作",
	CodeNotFound:            "资源不存在",
	CodeTooManyRequests:     "操作过于频繁，请稍后再试",
	CodeInsufficientBalance: "账户余额不足",
	CodeInvalidOrderState:   "当前订单状态不支持该操作",
	CodeUpstream:            "上游服务暂时不可用",
	CodeInternal:            "服务暂时不可用",
}

var httpStatus = map[Code]int{
	CodeSuccess:             http.StatusOK,
	CodeInvalidParam:        http.StatusBadRequest,
	CodeUnauthorized:        http.StatusUnauthorized,
	CodeForbidden:           http.StatusForbidden,
	CodeNotFound:            http.StatusNotFound,
	CodeTooManyRequests:     http.StatusTooManyRequests,
	CodeInsufficientBalance: http.StatusBadRequest,
	CodeInvalidOrderState:   http.StatusConflict,
	CodeUpstream:            http.StatusBadGateway,
	CodeInternal:            http.StatusInternalServerError,
}

// Message 返回错误码的默认提示。未登记的错误码返回通用提示。
func (c Code) Message() string {
	if msg, ok := messages[c]; ok {
		return msg
	}
	return messages[CodeInternal]
}

// HTTPStatus 返回错误码对应的 HTTP 状态码。
func (c Code) HTTPStatus() int {
	if status, ok := httpStatus[c]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// Error 是携带业务码的错误。
//
// 底层原因通过 Unwrap 暴露，只用于服务端日志，不会返回给客户端，
// 避免把数据库错误、上游响应等内部细节泄露出去。
type Error struct {
	Code    Code
	Message string
	Fields  map[string]string // 字段级校验详情，可为空
	cause   error
}

// New 用错误码的默认提示构造错误。
func New(code Code) *Error {
	return &Error{Code: code, Message: code.Message()}
}

// Newf 用自定义提示构造错误。
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// Unwrap 暴露底层错误，供 errors.Is / errors.As 使用。
func (e *Error) Unwrap() error { return e.cause }

// WithCause 附加底层错误，仅用于日志。
func (e *Error) WithCause(err error) *Error {
	clone := *e
	clone.cause = err
	return &clone
}

// WithField 附加单个字段的校验错误。
func (e *Error) WithField(field, reason string) *Error {
	clone := *e
	clone.Fields = make(map[string]string, len(e.Fields)+1)
	for k, v := range e.Fields {
		clone.Fields[k] = v
	}
	clone.Fields[field] = reason
	return &clone
}

// WithFields 附加多个字段的校验错误。
func (e *Error) WithFields(fields map[string]string) *Error {
	clone := *e
	clone.Fields = make(map[string]string, len(e.Fields)+len(fields))
	for k, v := range e.Fields {
		clone.Fields[k] = v
	}
	for k, v := range fields {
		clone.Fields[k] = v
	}
	return &clone
}

// From 把任意 error 归一化为 *Error。
// 非 *Error 一律视为服务端内部错误，不向客户端暴露原始内容。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(CodeInternal).WithCause(err)
}
