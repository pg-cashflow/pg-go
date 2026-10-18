package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ListProperties handles GET /owner/properties — returns all properties owned by the authenticated owner.
func (h *Handlers) ListProperties(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	var phone, email string
	if h.UserStore != nil {
		if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			phone = u.Phone
			email = u.Email
		}
	} else if h.AuthUserRepo != nil {
		if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			phone = u.Phone
			email = u.Email
		}
	}
	all, err := h.PropertyStore.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	out := make([]domain.Property, 0, len(all))
	for _, p := range all {
		isMatch := false
		if claims.PropertyID != nil && p.ID == *claims.PropertyID {
			isMatch = true
		} else if phone != "" && p.OwnerPhone == phone {
			isMatch = true
		} else if email != "" && strings.EqualFold(p.OwnerEmail, email) {
			isMatch = true
		}
		if isMatch {
			p.UPIVPA = "" // never expose in API
			out = append(out, p)
		}
	}
	c.JSON(http.StatusOK, gin.H{"properties": out})
}

// CreateFloorWithRoomsInput defines custom floor details and room number assignments.
type CreateFloorWithRoomsInput struct {
	FloorNumber int      `json:"floor_number"`
	Name        string   `json:"name"`
	Rooms       []string `json:"rooms"`
	Capacity    int16    `json:"capacity"`
}

type createPropertyBody struct {
	Name            string                      `json:"name" binding:"required"`
	Address         string                      `json:"address"`
	OwnerName       string                      `json:"owner_name"`
	PaymentMode     string                      `json:"payment_mode"`
	FloorsCount     int                         `json:"floors_count"`      // e.g. 3
	TotalRoomsCount int                         `json:"total_rooms_count"` // e.g. 12
	RoomsPerFloor   int                         `json:"rooms_per_floor"`   // e.g. 4
	RoomCapacity    int16                       `json:"room_capacity"`     // default 2
	StartFloor      *int                        `json:"start_floor"`       // default 1 (or 0 for Ground)
	CustomFloors    []CreateFloorWithRoomsInput `json:"custom_floors,omitempty"`
}

// generateFloorsAndRooms creates floors and rooms in bulk for a property.
func generateFloorsAndRooms(
	ctx context.Context,
	store gamification.Store,
	propID uuid.UUID,
	floorsCount, totalRooms, roomsPerFloor int,
	roomCap int16,
	startFloorPtr *int,
	customFloors []CreateFloorWithRoomsInput,
) (int, int, error) {
	if store == nil {
		return 0, 0, nil
	}
	createdFloors := 0
	createdRooms := 0

	if len(customFloors) > 0 {
		defaultCap := roomCap
		if defaultCap <= 0 {
			defaultCap = 2
		}
		for _, fl := range customFloors {
			fName := strings.TrimSpace(fl.Name)
			if fName == "" {
				if fl.FloorNumber == 0 {
					fName = "Ground Floor"
				} else {
					fName = fmt.Sprintf("Floor %d", fl.FloorNumber)
				}
			}
			floorObj := domain.Floor{
				PropertyID:  propID,
				FloorNumber: fl.FloorNumber,
				Name:        fName,
			}
			if err := store.CreateFloor(ctx, &floorObj); err != nil {
				return createdFloors, createdRooms, err
			}
			createdFloors++

			cap := fl.Capacity
			if cap <= 0 {
				cap = defaultCap
			}
			for _, rNum := range fl.Rooms {
				rTrim := strings.TrimSpace(rNum)
				if rTrim == "" {
					continue
				}
				roomObj := domain.Room{
					PropertyID:    propID,
					FloorId:       floorObj.ID,
					RoomNumber:    rTrim,
					Capacity:      cap,
					IncludedUnits: 50,
				}
				if err := store.CreateRoom(ctx, &roomObj); err == nil {
					createdRooms++
				}
			}
		}
		return createdFloors, createdRooms, nil
	}

	if floorsCount <= 0 {
		return 0, 0, nil
	}

	if totalRooms <= 0 && roomsPerFloor > 0 {
		totalRooms = floorsCount * roomsPerFloor
	}
	if totalRooms <= 0 {
		totalRooms = floorsCount * 4
	}

	startFloor := 1
	if startFloorPtr != nil {
		startFloor = *startFloorPtr
	}
	defaultCap := roomCap
	if defaultCap <= 0 {
		defaultCap = 2
	}

	basePerFloor := totalRooms / floorsCount
	extra := totalRooms % floorsCount

	for i := 0; i < floorsCount; i++ {
		flNum := startFloor + i
		flName := fmt.Sprintf("Floor %d", flNum)
		if flNum == 0 {
			flName = "Ground Floor"
		}
		floorObj := domain.Floor{
			PropertyID:  propID,
			FloorNumber: flNum,
			Name:        flName,
		}
		if err := store.CreateFloor(ctx, &floorObj); err != nil {
			return createdFloors, createdRooms, err
		}
		createdFloors++

		roomsOnThisFloor := basePerFloor
		if i < extra {
			roomsOnThisFloor++
		}

		for r := 1; r <= roomsOnThisFloor; r++ {
			var roomNum string
			if flNum == 0 {
				roomNum = fmt.Sprintf("G%02d", r)
			} else {
				roomNum = fmt.Sprintf("%d%02d", flNum, r)
			}
			roomObj := domain.Room{
				PropertyID:    propID,
				FloorId:       floorObj.ID,
				RoomNumber:    roomNum,
				Capacity:      defaultCap,
				IncludedUnits: 50,
			}
			if err := store.CreateRoom(ctx, &roomObj); err == nil {
				createdRooms++
			}
		}
	}
	return createdFloors, createdRooms, nil
}

// CreateProperty handles POST /owner/properties — creates an additional property for the authenticated owner,
// with optional bulk floor & room generation.
func (h *Handlers) CreateProperty(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
		return
	}
	var body createPropertyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid property body", apierr.CodeRequestInvalidBody)
		return
	}

	var userPhone, userEmail string
	if h.UserStore != nil {
		if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			userPhone = u.Phone
			userEmail = u.Email
		}
	} else if h.AuthUserRepo != nil {
		if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			userPhone = u.Phone
			userEmail = u.Email
		}
	}

	prop := domain.Property{
		Name:        strings.TrimSpace(body.Name),
		OwnerPhone:  userPhone,
		OwnerEmail:  userEmail,
		OwnerName:   strings.TrimSpace(body.OwnerName),
		PaymentMode: body.PaymentMode,
	}
	if body.Address != "" {
		trimmed := strings.TrimSpace(body.Address)
		prop.Address = &trimmed
	}
	if err := h.PropertyStore.Create(c.Request.Context(), &prop); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create property failed"})
		return
	}

	floorsCreated, roomsCreated, _ := generateFloorsAndRooms(
		c.Request.Context(),
		h.GamificationStore,
		prop.ID,
		body.FloorsCount,
		body.TotalRoomsCount,
		body.RoomsPerFloor,
		body.RoomCapacity,
		body.StartFloor,
		body.CustomFloors,
	)

	prop.UPIVPA = ""
	c.JSON(http.StatusCreated, gin.H{
		"property":       prop,
		"floors_created": floorsCreated,
		"rooms_created":  roomsCreated,
	})
}

// OwnerSwitchProperty handles POST /owner/properties/:id/switch — switches active property for single login.
func (h *Handlers) OwnerSwitchProperty(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
		return
	}
	propIDStr := c.Param("id")
	targetID, err := uuid.Parse(propIDStr)
	if err != nil || targetID == uuid.Nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid property id", apierr.CodeRequestInvalidId)
		return
	}

	prop, err := h.PropertyStore.GetByID(c.Request.Context(), targetID)
	if err != nil || prop == nil {
		apierr.RespondClientErr(c, http.StatusForbidden, "property not found or not owned", apierr.CodeAuthForbidden)
		return
	}

	var userPhone, userEmail string
	var user *domain.User
	if h.UserStore != nil {
		user, _ = h.UserStore.GetByID(c.Request.Context(), claims.UserID)
	} else if h.AuthUserRepo != nil {
		user, _ = h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID)
	}
	if user != nil {
		userPhone = user.Phone
		userEmail = user.Email
	}

	authorized := false
	if claims.PropertyID != nil && *claims.PropertyID == targetID {
		authorized = true
	} else if userPhone != "" && prop.OwnerPhone == userPhone {
		authorized = true
	} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
		authorized = true
	}

	if !authorized {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
		return
	}

	if h.UserStore != nil {
		if err := h.UserStore.SetPropertyID(c.Request.Context(), claims.UserID, targetID); err != nil {
			respondErr(c, err)
			return
		}
	}

	prop.UPIVPA = ""
	c.JSON(http.StatusOK, gin.H{"ok": true, "property": prop})
}

// ResolveOwnerPropertyScope inspects X-Property-ID header or property_id query param
// and binds the verified property ID into requestscope.
func (h *Handlers) ResolveOwnerPropertyScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := auth.ClaimsFromContext(c)
		if !ok || claims.Role != domain.RoleOwner {
			c.Next()
			return
		}

		targetStr := c.GetHeader("X-Property-ID")
		if targetStr == "" {
			targetStr = c.Query("property_id")
		}

		if targetStr == "" {
			if claims.PropertyID != nil && *claims.PropertyID != uuid.Nil {
				c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), *claims.PropertyID))
			}
			c.Next()
			return
		}

		targetID, err := uuid.Parse(strings.TrimSpace(targetStr))
		if err != nil || targetID == uuid.Nil {
			apierr.RespondClientErr(c, http.StatusBadRequest, "invalid property id", apierr.CodeRequestInvalidId)
			c.Abort()
			return
		}

		if claims.PropertyID != nil && *claims.PropertyID == targetID {
			c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), targetID))
			c.Next()
			return
		}

		// Fail closed: a cross-property switch must be verified against the property
		// store. Without a store we cannot prove ownership, so the request is refused
		// rather than binding an arbitrary property ID into the request scope.
		if h.PropertyStore == nil {
			apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}
		prop, err := h.PropertyStore.GetByID(c.Request.Context(), targetID)
		if err != nil || prop == nil {
			apierr.RespondClientErr(c, http.StatusForbidden, "property not found or not owned", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}

		var userPhone, userEmail string
		if h.UserStore != nil {
			if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
				userPhone = u.Phone
				userEmail = u.Email
			}
		} else if h.AuthUserRepo != nil {
			if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
				userPhone = u.Phone
				userEmail = u.Email
			}
		}

		authorized := false
		if userPhone != "" && prop.OwnerPhone == userPhone {
			authorized = true
		} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
			authorized = true
		}

		if !authorized {
			apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}

		c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), targetID))
		c.Next()
	}
}
