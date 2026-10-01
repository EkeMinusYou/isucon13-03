package main

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

type PaymentResult struct {
	TotalTip int64 `json:"total_tip"`
}

func GetPaymentResult(c echo.Context) error {
	store := currentMutations.Load()
	if store == nil {
		return postsUnavailable("initialization is required")
	}
	total, err := store.payment()
	if err != nil {
		return mutationHTTPError(err)
	}
	return c.JSON(http.StatusOK, &PaymentResult{TotalTip: total})
}
