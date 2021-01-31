package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
	"github.com/knz/strtime"
	"github.com/lithammer/shortuuid/v3"
	"gopkg.in/ini.v1"
)

func respond(code int, message string, gc *gin.Context) {
	resp := stringResponse{}
	if code == 200 || code == 204 {
		resp.Response = message
	} else {
		resp.Error = message
	}
	gc.JSON(code, resp)
	gc.Abort()
}

func respondBool(code int, val bool, gc *gin.Context) {
	resp := boolResponse{}
	if !val {
		resp.Error = true
	} else {
		resp.Success = true
	}
	gc.JSON(code, resp)
	gc.Abort()
}

func (app *appContext) loadStrftime() {
	app.datePattern = app.config.Section("email").Key("date_format").String()
	app.timePattern = `%H:%M`
	if val, _ := app.config.Section("email").Key("use_24h").Bool(); !val {
		app.timePattern = `%I:%M %p`
	}
	return
}

func (app *appContext) prettyTime(dt time.Time) (date, time string) {
	date, _ = strtime.Strftime(dt, app.datePattern)
	time, _ = strtime.Strftime(dt, app.timePattern)
	return
}

func (app *appContext) formatDatetime(dt time.Time) string {
	d, t := app.prettyTime(dt)
	return d + " " + t
}

// https://stackoverflow.com/questions/36530251/time-since-with-months-and-years/36531443#36531443 THANKS
func timeDiff(a, b time.Time) (year, month, day, hour, min, sec int) {
	if a.Location() != b.Location() {
		b = b.In(a.Location())
	}
	if a.After(b) {
		a, b = b, a
	}
	y1, M1, d1 := a.Date()
	y2, M2, d2 := b.Date()

	h1, m1, s1 := a.Clock()
	h2, m2, s2 := b.Clock()

	year = int(y2 - y1)
	month = int(M2 - M1)
	day = int(d2 - d1)
	hour = int(h2 - h1)
	min = int(m2 - m1)
	sec = int(s2 - s1)

	// Normalize negative values
	if sec < 0 {
		sec += 60
		min--
	}
	if min < 0 {
		min += 60
		hour--
	}
	if hour < 0 {
		hour += 24
		day--
	}
	if day < 0 {
		// days in month:
		t := time.Date(y1, M1, 32, 0, 0, 0, 0, time.UTC)
		day += 32 - t.Day()
		month--
	}
	if month < 0 {
		month += 12
		year--
	}
	return
}

func (app *appContext) checkInvites() {
	currentTime := time.Now()
	app.storage.loadInvites()
	changed := false
	for code, data := range app.storage.invites {
		expiry := data.ValidTill
		if !currentTime.After(expiry) {
			continue
		}
		app.debug.Printf("Housekeeping: Deleting old invite %s", code)
		notify := data.Notify
		if emailEnabled && app.config.Section("notifications").Key("enabled").MustBool(false) && len(notify) != 0 {
			app.debug.Printf("%s: Expiry notification", code)
			var wait sync.WaitGroup
			for address, settings := range notify {
				if !settings["notify-expiry"] {
					continue
				}
				wait.Add(1)
				go func(addr string) {
					defer wait.Done()
					msg, err := app.email.constructExpiry(code, data, app)
					if err != nil {
						app.err.Printf("%s: Failed to construct expiry notification", code)
						app.debug.Printf("Error: %s", err)
					} else if err := app.email.send(addr, msg); err != nil {
						app.err.Printf("%s: Failed to send expiry notification", code)
						app.debug.Printf("Error: %s", err)
					} else {
						app.info.Printf("Sent expiry notification to %s", addr)
					}
				}(address)
			}
			wait.Wait()
		}
		changed = true
		delete(app.storage.invites, code)
	}
	if changed {
		app.storage.storeInvites()
	}
}

func (app *appContext) checkInvite(code string, used bool, username string) bool {
	currentTime := time.Now()
	app.storage.loadInvites()
	changed := false
	inv, match := app.storage.invites[code]
	if !match {
		return false
	}
	expiry := inv.ValidTill
	if currentTime.After(expiry) {
		app.debug.Printf("Housekeeping: Deleting old invite %s", code)
		notify := inv.Notify
		if emailEnabled && app.config.Section("notifications").Key("enabled").MustBool(false) && len(notify) != 0 {
			app.debug.Printf("%s: Expiry notification", code)
			for address, settings := range notify {
				if settings["notify-expiry"] {
					go func() {
						msg, err := app.email.constructExpiry(code, inv, app)
						if err != nil {
							app.err.Printf("%s: Failed to construct expiry notification", code)
							app.debug.Printf("Error: %s", err)
						} else if err := app.email.send(address, msg); err != nil {
							app.err.Printf("%s: Failed to send expiry notification", code)
							app.debug.Printf("Error: %s", err)
						} else {
							app.info.Printf("Sent expiry notification to %s", address)
						}
					}()
				}
			}
		}
		changed = true
		match = false
		delete(app.storage.invites, code)
	} else if used {
		changed = true
		del := false
		newInv := inv
		if newInv.RemainingUses == 1 {
			del = true
			delete(app.storage.invites, code)
		} else if newInv.RemainingUses != 0 {
			// 0 means infinite i guess?
			newInv.RemainingUses--
		}
		newInv.UsedBy = append(newInv.UsedBy, []string{username, app.formatDatetime(currentTime)})
		if !del {
			app.storage.invites[code] = newInv
		}
	}
	if changed {
		app.storage.storeInvites()
	}
	return match
}

func (app *appContext) getOmbiUser(jfID string) (map[string]interface{}, int, error) {
	ombiUsers, code, err := app.ombi.GetUsers()
	if err != nil || code != 200 {
		return nil, code, err
	}
	jfUser, code, err := app.jf.UserByID(jfID, false)
	if err != nil || code != 200 {
		return nil, code, err
	}
	username := jfUser["Name"].(string)
	email := ""
	if e, ok := app.storage.emails[jfID]; ok {
		email = e.(string)
	}
	for _, ombiUser := range ombiUsers {
		ombiAddr := ""
		if a, ok := ombiUser["emailAddress"]; ok && a != nil {
			ombiAddr = a.(string)
		}
		if ombiUser["userName"].(string) == username || (ombiAddr == email && email != "") {
			return ombiUser, code, err
		}
	}
	return nil, 400, fmt.Errorf("Couldn't find user")
}

// Routes from now on!

// @Summary Creates a new Jellyfin user without an invite.
// @Produce json
// @Param newUserDTO body newUserDTO true "New user request object"
// @Success 200
// @Router /users [post]
// @Security Bearer
// @tags Users
func (app *appContext) NewUserAdmin(gc *gin.Context) {
	respondUser := func(code int, user, email bool, msg string, gc *gin.Context) {
		resp := newUserResponse{
			User:  user,
			Email: email,
			Error: msg,
		}
		gc.JSON(code, resp)
		gc.Abort()
	}
	var req newUserDTO
	gc.BindJSON(&req)
	existingUser, _, _ := app.jf.UserByName(req.Username, false)
	if existingUser != nil {
		msg := fmt.Sprintf("User already exists named %s", req.Username)
		app.info.Printf("%s New user failed: %s", req.Username, msg)
		respondUser(401, false, false, msg, gc)
		return
	}
	user, status, err := app.jf.NewUser(req.Username, req.Password)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("%s New user failed: Jellyfin responded with %d", req.Username, status)
		respondUser(401, false, false, "Unknown error", gc)
		return
	}
	var id string
	if user["Id"] != nil {
		id = user["Id"].(string)
	}
	if len(app.storage.policy) != 0 {
		status, err = app.jf.SetPolicy(id, app.storage.policy)
		if !(status == 200 || status == 204 || err == nil) {
			app.err.Printf("%s: Failed to set user policy: Code %d", req.Username, status)
			app.debug.Printf("%s: Error: %s", req.Username, err)
		}
	}
	if len(app.storage.configuration) != 0 && len(app.storage.displayprefs) != 0 {
		status, err = app.jf.SetConfiguration(id, app.storage.configuration)
		if (status == 200 || status == 204) && err == nil {
			status, err = app.jf.SetDisplayPreferences(id, app.storage.displayprefs)
		}
		if !((status == 200 || status == 204) && err == nil) {
			app.err.Printf("%s: Failed to set configuration template: Code %d", req.Username, status)
		}
	}
	app.jf.CacheExpiry = time.Now()
	if app.config.Section("password_resets").Key("enabled").MustBool(false) {
		app.storage.emails[id] = req.Email
		app.storage.storeEmails()
	}
	if app.config.Section("ombi").Key("enabled").MustBool(false) {
		app.storage.loadOmbiTemplate()
		if len(app.storage.ombi_template) != 0 {
			errors, code, err := app.ombi.NewUser(req.Username, req.Password, req.Email, app.storage.ombi_template)
			if err != nil || code != 200 {
				app.info.Printf("Failed to create Ombi user (%d): %s", code, err)
				app.debug.Printf("Errors reported by Ombi: %s", strings.Join(errors, ", "))
			} else {
				app.info.Println("Created Ombi user")
			}
		}
	}
	if emailEnabled && app.config.Section("welcome_email").Key("enabled").MustBool(false) && req.Email != "" {
		app.debug.Printf("%s: Sending welcome email to %s", req.Username, req.Email)
		msg, err := app.email.constructWelcome(req.Username, app)
		if err != nil {
			app.err.Printf("%s: Failed to construct welcome email: %s", req.Username, err)
			respondUser(500, true, false, err.Error(), gc)
			return
		} else if err := app.email.send(req.Email, msg); err != nil {
			app.err.Printf("%s: Failed to send welcome email: %s", req.Username, err)
			respondUser(500, true, false, err.Error(), gc)
			return
		} else {
			app.info.Printf("%s: Sent welcome email to %s", req.Username, req.Email)
		}
	}
	respondUser(200, true, true, "", gc)
}

type errorFunc func(gc *gin.Context)

func (app *appContext) newUser(req newUserDTO, confirmed bool) (f errorFunc, success bool) {
	existingUser, _, _ := app.jf.UserByName(req.Username, false)
	if existingUser != nil {
		f = func(gc *gin.Context) {
			msg := fmt.Sprintf("User %s already exists", req.Username)
			app.info.Printf("%s: New user failed: %s", req.Code, msg)
			respond(401, "errorUserExists", gc)
		}
		success = false
		return
	}
	if emailEnabled && app.config.Section("email_confirmation").Key("enabled").MustBool(false) && !confirmed {
		claims := jwt.MapClaims{
			"valid":    true,
			"invite":   req.Code,
			"email":    req.Email,
			"username": req.Username,
			"password": req.Password,
			"exp":      strconv.FormatInt(time.Now().Add(time.Hour*12).Unix(), 10),
			"type":     "confirmation",
		}
		tk := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		key, err := tk.SignedString([]byte(os.Getenv("JFA_SECRET")))
		if err != nil {
			f = func(gc *gin.Context) {
				app.info.Printf("Failed to generate confirmation token: %v", err)
				respond(500, "errorUnknown", gc)
			}
			success = false
			return
		}
		inv := app.storage.invites[req.Code]
		inv.Keys = append(inv.Keys, key)
		app.storage.invites[req.Code] = inv
		app.storage.storeInvites()
		f = func(gc *gin.Context) {
			app.debug.Printf("%s: Email confirmation required", req.Code)
			respond(401, "confirmEmail", gc)
			msg, err := app.email.constructConfirmation(req.Code, req.Username, key, app)
			if err != nil {
				app.err.Printf("%s: Failed to construct confirmation email", req.Code)
				app.debug.Printf("%s: Error: %s", req.Code, err)
			} else if err := app.email.send(req.Email, msg); err != nil {
				app.err.Printf("%s: Failed to send user confirmation email: %s", req.Code, err)
			} else {
				app.info.Printf("%s: Sent user confirmation email to %s", req.Code, req.Email)
			}
		}
		success = false
		return
	}

	user, status, err := app.jf.NewUser(req.Username, req.Password)
	if !(status == 200 || status == 204) || err != nil {
		f = func(gc *gin.Context) {
			app.err.Printf("%s New user failed: Jellyfin responded with %d", req.Code, status)
			respond(401, app.storage.lang.Admin[app.storage.lang.chosenAdminLang].Notifications.get("errorUnknown"), gc)
		}
		success = false
		return
	}
	app.storage.loadProfiles()
	invite := app.storage.invites[req.Code]
	app.checkInvite(req.Code, true, req.Username)
	if emailEnabled && app.config.Section("notifications").Key("enabled").MustBool(false) {
		for address, settings := range invite.Notify {
			if settings["notify-creation"] {
				go func() {
					msg, err := app.email.constructCreated(req.Code, req.Username, req.Email, invite, app)
					if err != nil {
						app.err.Printf("%s: Failed to construct user creation notification", req.Code)
						app.debug.Printf("%s: Error: %s", req.Code, err)
					} else if err := app.email.send(address, msg); err != nil {
						app.err.Printf("%s: Failed to send user creation notification", req.Code)
						app.debug.Printf("%s: Error: %s", req.Code, err)
					} else {
						app.info.Printf("%s: Sent user creation notification to %s", req.Code, address)
					}
				}()
			}
		}
	}
	var id string
	if user["Id"] != nil {
		id = user["Id"].(string)
	}
	if invite.Profile != "" {
		app.debug.Printf("Applying settings from profile \"%s\"", invite.Profile)
		profile, ok := app.storage.profiles[invite.Profile]
		if !ok {
			profile = app.storage.profiles["Default"]
		}
		if len(profile.Policy) != 0 {
			app.debug.Printf("Applying policy from profile \"%s\"", invite.Profile)
			status, err = app.jf.SetPolicy(id, profile.Policy)
			if !((status == 200 || status == 204) && err == nil) {
				app.err.Printf("%s: Failed to set user policy: Code %d", req.Code, status)
				app.debug.Printf("%s: Error: %s", req.Code, err)
			}
		}
		if len(profile.Configuration) != 0 && len(profile.Displayprefs) != 0 {
			app.debug.Printf("Applying homescreen from profile \"%s\"", invite.Profile)
			status, err = app.jf.SetConfiguration(id, profile.Configuration)
			if (status == 200 || status == 204) && err == nil {
				status, err = app.jf.SetDisplayPreferences(id, profile.Displayprefs)
			}
			if !((status == 200 || status == 204) && err == nil) {
				app.err.Printf("%s: Failed to set configuration template: Code %d", req.Code, status)
				app.debug.Printf("%s: Error: %s", req.Code, err)
			}
		}
	}
	// if app.config.Section("password_resets").Key("enabled").MustBool(false) {
	if req.Email != "" {
		app.storage.emails[id] = req.Email
		app.storage.storeEmails()
	}
	if app.config.Section("ombi").Key("enabled").MustBool(false) {
		app.storage.loadOmbiTemplate()
		if len(app.storage.ombi_template) != 0 {
			errors, code, err := app.ombi.NewUser(req.Username, req.Password, req.Email, app.storage.ombi_template)
			if err != nil || code != 200 {
				app.info.Printf("Failed to create Ombi user (%d): %s", code, err)
				app.debug.Printf("Errors reported by Ombi: %s", strings.Join(errors, ", "))
			} else {
				app.info.Println("Created Ombi user")
			}
		}
	}
	if emailEnabled && app.config.Section("welcome_email").Key("enabled").MustBool(false) && req.Email != "" {
		app.debug.Printf("%s: Sending welcome email to %s", req.Username, req.Email)
		msg, err := app.email.constructWelcome(req.Username, app)
		if err != nil {
			app.err.Printf("%s: Failed to construct welcome email: %s", req.Username, err)
		} else if err := app.email.send(req.Email, msg); err != nil {
			app.err.Printf("%s: Failed to send welcome email: %s", req.Username, err)
		} else {
			app.info.Printf("%s: Sent welcome email to %s", req.Username, req.Email)
		}
	}
	success = true
	return
}

// @Summary Creates a new Jellyfin user via invite code
// @Produce json
// @Param newUserDTO body newUserDTO true "New user request object"
// @Success 200 {object} PasswordValidation
// @Failure 400 {object} PasswordValidation
// @Router /newUser [post]
// @tags Users
func (app *appContext) NewUser(gc *gin.Context) {
	var req newUserDTO
	gc.BindJSON(&req)
	app.debug.Printf("%s: New user attempt", req.Code)
	if !app.checkInvite(req.Code, false, "") {
		app.info.Printf("%s New user failed: invalid code", req.Code)
		respond(401, "errorInvalidCode", gc)
		return
	}
	validation := app.validator.validate(req.Password)
	valid := true
	for _, val := range validation {
		if !val {
			valid = false
		}
	}
	if !valid {
		// 200 bcs idk what i did in js
		app.info.Printf("%s New user failed: Invalid password", req.Code)
		gc.JSON(200, validation)
		return
	}
	f, success := app.newUser(req, false)
	if !success {
		f(gc)
		return
	}
	code := 200
	for _, val := range validation {
		if !val {
			code = 400
		}
	}
	gc.JSON(code, validation)
}

// @Summary Delete a list of users, optionally notifying them why.
// @Produce json
// @Param deleteUserDTO body deleteUserDTO true "User deletion request object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} stringResponse
// @Failure 500 {object} errorListDTO "List of errors"
// @Router /users [delete]
// @Security Bearer
// @tags Users
func (app *appContext) DeleteUser(gc *gin.Context) {
	var req deleteUserDTO
	gc.BindJSON(&req)
	errors := map[string]string{}
	ombiEnabled := app.config.Section("ombi").Key("enabled").MustBool(false)
	for _, userID := range req.Users {
		if ombiEnabled {
			ombiUser, code, err := app.getOmbiUser(userID)
			if code == 200 && err == nil {
				if id, ok := ombiUser["id"]; ok {
					status, err := app.ombi.DeleteUser(id.(string))
					if err != nil || status != 200 {
						app.err.Printf("Failed to delete ombi user: %d %s", status, err)
						errors[userID] = fmt.Sprintf("Ombi: %d %s, ", status, err)
					}
				}
			}
		}
		status, err := app.jf.DeleteUser(userID)
		if !(status == 200 || status == 204) || err != nil {
			msg := fmt.Sprintf("%d: %s", status, err)
			if _, ok := errors[userID]; !ok {
				errors[userID] = msg
			} else {
				errors[userID] += msg
			}
		}
		if emailEnabled && req.Notify {
			addr, ok := app.storage.emails[userID]
			if addr != nil && ok {
				go func(userID, reason, address string) {
					msg, err := app.email.constructDeleted(reason, app)
					if err != nil {
						app.err.Printf("%s: Failed to construct account deletion email", userID)
						app.debug.Printf("%s: Error: %s", userID, err)
					} else if err := app.email.send(address, msg); err != nil {
						app.err.Printf("%s: Failed to send to %s", userID, address)
						app.debug.Printf("%s: Error: %s", userID, err)
					} else {
						app.info.Printf("%s: Sent deletion email to %s", userID, address)
					}
				}(userID, req.Reason, addr.(string))
			}
		}
	}
	app.jf.CacheExpiry = time.Now()
	if len(errors) == len(req.Users) {
		respondBool(500, false, gc)
		app.err.Printf("Account deletion failed: %s", errors[req.Users[0]])
		return
	} else if len(errors) != 0 {
		gc.JSON(500, errors)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Create a new invite.
// @Produce json
// @Param generateInviteDTO body generateInviteDTO true "New invite request object"
// @Success 200 {object} boolResponse
// @Router /invites [post]
// @Security Bearer
// @tags Invites
func (app *appContext) GenerateInvite(gc *gin.Context) {
	var req generateInviteDTO
	app.debug.Println("Generating new invite")
	app.storage.loadInvites()
	gc.BindJSON(&req)
	currentTime := time.Now()
	validTill := currentTime.AddDate(0, 0, req.Days)
	validTill = validTill.Add(time.Hour*time.Duration(req.Hours) + time.Minute*time.Duration(req.Minutes))
	// make sure code doesn't begin with number
	inviteCode := shortuuid.New()
	_, err := strconv.Atoi(string(inviteCode[0]))
	for err == nil {
		inviteCode = shortuuid.New()
		_, err = strconv.Atoi(string(inviteCode[0]))
	}
	var invite Invite
	if req.Label != "" {
		invite.Label = req.Label
	}
	invite.Created = currentTime
	if req.MultipleUses {
		if req.NoLimit {
			invite.NoLimit = true
		} else {
			invite.RemainingUses = req.RemainingUses
		}
	} else {
		invite.RemainingUses = 1
	}
	invite.ValidTill = validTill
	if emailEnabled && req.Email != "" && app.config.Section("invite_emails").Key("enabled").MustBool(false) {
		app.debug.Printf("%s: Sending invite email", inviteCode)
		invite.Email = req.Email
		msg, err := app.email.constructInvite(inviteCode, invite, app)
		if err != nil {
			invite.Email = fmt.Sprintf("Failed to send to %s", req.Email)
			app.err.Printf("%s: Failed to construct invite email", inviteCode)
			app.debug.Printf("%s: Error: %s", inviteCode, err)
		} else if err := app.email.send(req.Email, msg); err != nil {
			invite.Email = fmt.Sprintf("Failed to send to %s", req.Email)
			app.err.Printf("%s: %s", inviteCode, invite.Email)
			app.debug.Printf("%s: Error: %s", inviteCode, err)
		} else {
			app.info.Printf("%s: Sent invite email to %s", inviteCode, req.Email)
		}
	}
	if req.Profile != "" {
		if _, ok := app.storage.profiles[req.Profile]; ok {
			invite.Profile = req.Profile
		} else {
			invite.Profile = "Default"
		}
	}
	app.storage.invites[inviteCode] = invite
	app.storage.storeInvites()
	respondBool(200, true, gc)
}

// @Summary Set profile for an invite
// @Produce json
// @Param inviteProfileDTO body inviteProfileDTO true "Invite profile object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /invites/profile [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) SetProfile(gc *gin.Context) {
	var req inviteProfileDTO
	gc.BindJSON(&req)
	app.debug.Printf("%s: Setting profile to \"%s\"", req.Invite, req.Profile)
	// "" means "Don't apply profile"
	if _, ok := app.storage.profiles[req.Profile]; !ok && req.Profile != "" {
		app.err.Printf("%s: Profile \"%s\" not found", req.Invite, req.Profile)
		respond(500, "Profile not found", gc)
		return
	}
	inv := app.storage.invites[req.Invite]
	inv.Profile = req.Profile
	app.storage.invites[req.Invite] = inv
	app.storage.storeInvites()
	respondBool(200, true, gc)
}

// @Summary Get a list of profiles
// @Produce json
// @Success 200 {object} getProfilesDTO
// @Router /profiles [get]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) GetProfiles(gc *gin.Context) {
	app.storage.loadProfiles()
	app.debug.Println("Profiles requested")
	out := getProfilesDTO{
		DefaultProfile: app.storage.defaultProfile,
		Profiles:       map[string]profileDTO{},
	}
	for name, p := range app.storage.profiles {
		out.Profiles[name] = profileDTO{
			Admin:         p.Admin,
			LibraryAccess: p.LibraryAccess,
			FromUser:      p.FromUser,
		}
	}
	gc.JSON(200, out)
}

// @Summary Set the default profile to use.
// @Produce json
// @Param profileChangeDTO body profileChangeDTO true "Default profile object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /profiles/default [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) SetDefaultProfile(gc *gin.Context) {
	req := profileChangeDTO{}
	gc.BindJSON(&req)
	app.info.Printf("Setting default profile to \"%s\"", req.Name)
	if _, ok := app.storage.profiles[req.Name]; !ok {
		app.err.Printf("Profile not found: \"%s\"", req.Name)
		respond(500, "Profile not found", gc)
		return
	}
	for name, profile := range app.storage.profiles {
		if name == req.Name {
			profile.Admin = true
			app.storage.profiles[name] = profile
		} else {
			profile.Admin = false
		}
	}
	app.storage.defaultProfile = req.Name
	respondBool(200, true, gc)
}

// @Summary Create a profile based on a Jellyfin user's settings.
// @Produce json
// @Param newProfileDTO body newProfileDTO true "New profile object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /profiles [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) CreateProfile(gc *gin.Context) {
	app.info.Println("Profile creation requested")
	var req newProfileDTO
	gc.BindJSON(&req)
	user, status, err := app.jf.UserByID(req.ID, false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get user from Jellyfin: Code %d", status)
		app.debug.Printf("Error: %s", err)
		respond(500, "Couldn't get user", gc)
		return
	}
	profile := Profile{
		FromUser: user["Name"].(string),
		Policy:   user["Policy"].(map[string]interface{}),
	}
	app.debug.Printf("Creating profile from user \"%s\"", user["Name"].(string))
	if req.Homescreen {
		profile.Configuration = user["Configuration"].(map[string]interface{})
		profile.Displayprefs, status, err = app.jf.GetDisplayPreferences(req.ID)
		if !(status == 200 || status == 204) || err != nil {
			app.err.Printf("Failed to get DisplayPrefs: Code %d", status)
			app.debug.Printf("Error: %s", err)
			respond(500, "Couldn't get displayprefs", gc)
			return
		}
	}
	app.storage.loadProfiles()
	app.storage.profiles[req.Name] = profile
	app.storage.storeProfiles()
	app.storage.loadProfiles()
	respondBool(200, true, gc)
}

// @Summary Delete an existing profile
// @Produce json
// @Param profileChangeDTO body profileChangeDTO true "Delete profile object"
// @Success 200 {object} boolResponse
// @Router /profiles [delete]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) DeleteProfile(gc *gin.Context) {
	req := profileChangeDTO{}
	gc.BindJSON(&req)
	name := req.Name
	if _, ok := app.storage.profiles[name]; ok {
		if app.storage.defaultProfile == name {
			app.storage.defaultProfile = ""
		}
		delete(app.storage.profiles, name)
	}
	app.storage.storeProfiles()
	respondBool(200, true, gc)
}

// @Summary Get invites.
// @Produce json
// @Success 200 {object} getInvitesDTO
// @Router /invites [get]
// @Security Bearer
// @tags Invites
func (app *appContext) GetInvites(gc *gin.Context) {
	app.debug.Println("Invites requested")
	currentTime := time.Now()
	app.storage.loadInvites()
	app.checkInvites()
	var invites []inviteDTO
	for code, inv := range app.storage.invites {
		_, _, days, hours, minutes, _ := timeDiff(inv.ValidTill, currentTime)
		invite := inviteDTO{
			Code:    code,
			Days:    days,
			Hours:   hours,
			Minutes: minutes,
			Created: app.formatDatetime(inv.Created),
			Profile: inv.Profile,
			NoLimit: inv.NoLimit,
			Label:   inv.Label,
		}
		if len(inv.UsedBy) != 0 {
			invite.UsedBy = inv.UsedBy
		}
		invite.RemainingUses = 1
		if inv.RemainingUses != 0 {
			invite.RemainingUses = inv.RemainingUses
		}
		if inv.Email != "" {
			invite.Email = inv.Email
		}
		if len(inv.Notify) != 0 {
			var address string
			if app.config.Section("ui").Key("jellyfin_login").MustBool(false) {
				app.storage.loadEmails()
				if addr := app.storage.emails[gc.GetString("jfId")]; addr != nil {
					address = addr.(string)
				}
			} else {
				address = app.config.Section("ui").Key("email").String()
			}
			if _, ok := inv.Notify[address]; ok {
				if _, ok = inv.Notify[address]["notify-expiry"]; ok {
					invite.NotifyExpiry = inv.Notify[address]["notify-expiry"]
				}
				if _, ok = inv.Notify[address]["notify-creation"]; ok {
					invite.NotifyCreation = inv.Notify[address]["notify-creation"]
				}
			}
		}
		invites = append(invites, invite)
	}
	profiles := make([]string, len(app.storage.profiles))
	if len(app.storage.profiles) != 0 {
		profiles[0] = app.storage.defaultProfile
		i := 1
		if len(app.storage.profiles) > 1 {
			for p := range app.storage.profiles {
				if p != app.storage.defaultProfile {
					profiles[i] = p
					i++
				}
			}
		}
	}
	resp := getInvitesDTO{
		Profiles: profiles,
		Invites:  invites,
	}
	gc.JSON(200, resp)
}

// @Summary Set notification preferences for an invite.
// @Produce json
// @Param setNotifyDTO body setNotifyDTO true "Map of invite codes to notification settings objects"
// @Success 200
// @Failure 400 {object} stringResponse
// @Failure 500 {object} stringResponse
// @Router /invites/notify [post]
// @Security Bearer
// @tags Other
func (app *appContext) SetNotify(gc *gin.Context) {
	var req map[string]map[string]bool
	gc.BindJSON(&req)
	changed := false
	for code, settings := range req {
		app.debug.Printf("%s: Notification settings change requested", code)
		app.storage.loadInvites()
		app.storage.loadEmails()
		invite, ok := app.storage.invites[code]
		if !ok {
			app.err.Printf("%s Notification setting change failed: Invalid code", code)
			respond(400, "Invalid invite code", gc)
			return
		}
		var address string
		if app.config.Section("ui").Key("jellyfin_login").MustBool(false) {
			var ok bool
			address, ok = app.storage.emails[gc.GetString("jfId")].(string)
			if !ok {
				app.err.Printf("%s: Couldn't find email address. Make sure it's set", code)
				app.debug.Printf("%s: User ID \"%s\"", code, gc.GetString("jfId"))
				respond(500, "Missing user email", gc)
				return
			}
		} else {
			address = app.config.Section("ui").Key("email").String()
		}
		if invite.Notify == nil {
			invite.Notify = map[string]map[string]bool{}
		}
		if _, ok := invite.Notify[address]; !ok {
			invite.Notify[address] = map[string]bool{}
		} /*else {
		if _, ok := invite.Notify[address]["notify-expiry"]; !ok {
		*/
		if _, ok := settings["notify-expiry"]; ok && invite.Notify[address]["notify-expiry"] != settings["notify-expiry"] {
			invite.Notify[address]["notify-expiry"] = settings["notify-expiry"]
			app.debug.Printf("%s: Set \"notify-expiry\" to %t for %s", code, settings["notify-expiry"], address)
			changed = true
		}
		if _, ok := settings["notify-creation"]; ok && invite.Notify[address]["notify-creation"] != settings["notify-creation"] {
			invite.Notify[address]["notify-creation"] = settings["notify-creation"]
			app.debug.Printf("%s: Set \"notify-creation\" to %t for %s", code, settings["notify-creation"], address)
			changed = true
		}
		if changed {
			app.storage.invites[code] = invite
		}
	}
	if changed {
		app.storage.storeInvites()
	}
}

// @Summary Delete an invite.
// @Produce json
// @Param deleteInviteDTO body deleteInviteDTO true "Delete invite object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} stringResponse
// @Router /invites [delete]
// @Security Bearer
// @tags Invites
func (app *appContext) DeleteInvite(gc *gin.Context) {
	var req deleteInviteDTO
	gc.BindJSON(&req)
	app.debug.Printf("%s: Deletion requested", req.Code)
	var ok bool
	_, ok = app.storage.invites[req.Code]
	if ok {
		delete(app.storage.invites, req.Code)
		app.storage.storeInvites()
		app.info.Printf("%s: Invite deleted", req.Code)
		respondBool(200, true, gc)
		return
	}
	app.err.Printf("%s: Deletion failed: Invalid code", req.Code)
	respond(400, "Code doesn't exist", gc)
}

type dateToParse struct {
	Parsed time.Time `json:"parseme"`
}

func parseDT(date string) time.Time {
	// decent method
	dt, err := time.Parse("2006-01-02T15:04:05.000000", date)
	if err == nil {
		return dt
	}
	// emby method
	dt, err = time.Parse("2006-01-02T15:04:05.0000000+00:00", date)
	if err == nil {
		return dt
	}
	// magic method
	// some stored dates from jellyfin have no timezone at the end, if not we assume UTC
	if date[len(date)-1] != 'Z' {
		date += "Z"
	}
	timeJSON := []byte("{ \"parseme\": \"" + date + "\" }")
	var parsed dateToParse
	// Magically turn it into a time.Time
	json.Unmarshal(timeJSON, &parsed)
	return parsed.Parsed
}

// @Summary Get a list of Jellyfin users.
// @Produce json
// @Success 200 {object} getUsersDTO
// @Failure 500 {object} stringResponse
// @Router /users [get]
// @Security Bearer
// @tags Users
func (app *appContext) GetUsers(gc *gin.Context) {
	app.debug.Println("Users requested")
	var resp getUsersDTO
	resp.UserList = []respUser{}
	users, status, err := app.jf.GetUsers(false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get users from Jellyfin: Code %d", status)
		app.debug.Printf("Error: %s", err)
		respond(500, "Couldn't get users", gc)
		return
	}
	for _, jfUser := range users {
		var user respUser
		user.LastActive = "n/a"
		if jfUser["LastActivityDate"] != nil {
			date := parseDT(jfUser["LastActivityDate"].(string))
			user.LastActive = app.formatDatetime(date)
		}
		user.ID = jfUser["Id"].(string)
		user.Name = jfUser["Name"].(string)
		user.Admin = jfUser["Policy"].(map[string]interface{})["IsAdministrator"].(bool)
		if email, ok := app.storage.emails[jfUser["Id"].(string)]; ok {
			user.Email = email.(string)
		}

		resp.UserList = append(resp.UserList, user)
	}
	gc.JSON(200, resp)
}

// @Summary Get a list of Ombi users.
// @Produce json
// @Success 200 {object} ombiUsersDTO
// @Failure 500 {object} stringResponse
// @Router /ombi/users [get]
// @Security Bearer
// @tags Ombi
func (app *appContext) OmbiUsers(gc *gin.Context) {
	app.debug.Println("Ombi users requested")
	users, status, err := app.ombi.GetUsers()
	if err != nil || status != 200 {
		app.err.Printf("Failed to get users from Ombi: Code %d", status)
		app.debug.Printf("Error: %s", err)
		respond(500, "Couldn't get users", gc)
		return
	}
	userlist := make([]ombiUser, len(users))
	for i, data := range users {
		userlist[i] = ombiUser{
			Name: data["userName"].(string),
			ID:   data["id"].(string),
		}
	}
	gc.JSON(200, ombiUsersDTO{Users: userlist})
}

// @Summary Set new user defaults for Ombi accounts.
// @Produce json
// @Param ombiUser body ombiUser true "User to source settings from"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /ombi/defaults [post]
// @Security Bearer
// @tags Ombi
func (app *appContext) SetOmbiDefaults(gc *gin.Context) {
	var req ombiUser
	gc.BindJSON(&req)
	template, code, err := app.ombi.TemplateByID(req.ID)
	if err != nil || code != 200 || len(template) == 0 {
		app.err.Printf("Couldn't get user from Ombi: %d %s", code, err)
		respond(500, "Couldn't get user", gc)
		return
	}
	app.storage.ombi_template = template
	app.storage.storeOmbiTemplate()
	respondBool(200, true, gc)
}

// @Summary Modify user's email addresses.
// @Produce json
// @Param modifyEmailsDTO body modifyEmailsDTO true "Map of userIDs to email addresses"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /users/emails [post]
// @Security Bearer
// @tags Users
func (app *appContext) ModifyEmails(gc *gin.Context) {
	var req modifyEmailsDTO
	gc.BindJSON(&req)
	app.debug.Println("Email modification requested")
	users, status, err := app.jf.GetUsers(false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get users from Jellyfin: Code %d", status)
		app.debug.Printf("Error: %s", err)
		respond(500, "Couldn't get users", gc)
		return
	}
	ombiEnabled := app.config.Section("ombi").Key("enabled").MustBool(false)
	for _, jfUser := range users {
		id := jfUser["Id"].(string)
		if address, ok := req[id]; ok {
			app.storage.emails[jfUser["Id"].(string)] = address
			if ombiEnabled {
				ombiUser, code, err := app.getOmbiUser(id)
				if code == 200 && err == nil {
					ombiUser["emailAddress"] = address
					code, err = app.ombi.ModifyUser(ombiUser)
					if code != 200 || err != nil {
						app.err.Printf("%s: Failed to change ombi email address: %d %s", ombiUser["userName"].(string), code, err)
					}
				}
			}
		}
	}
	app.storage.storeEmails()
	app.info.Println("Email list modified")
	respondBool(200, true, gc)
}

// @Summary Apply settings to a list of users, either from a profile or from another user.
// @Produce json
// @Param userSettingsDTO body userSettingsDTO true "Parameters for applying settings"
// @Success 200 {object} errorListDTO
// @Failure 500 {object} errorListDTO "Lists of errors that occurred while applying settings"
// @Router /users/settings [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) ApplySettings(gc *gin.Context) {
	app.info.Println("User settings change requested")
	var req userSettingsDTO
	gc.BindJSON(&req)
	applyingFrom := "profile"
	var policy, configuration, displayprefs map[string]interface{}
	if req.From == "profile" {
		app.storage.loadProfiles()
		if _, ok := app.storage.profiles[req.Profile]; !ok || len(app.storage.profiles[req.Profile].Policy) == 0 {
			app.err.Printf("Couldn't find profile \"%s\" or profile was empty", req.Profile)
			respond(500, "Couldn't find profile", gc)
			return
		}
		if req.Homescreen {
			if len(app.storage.profiles[req.Profile].Configuration) == 0 || len(app.storage.profiles[req.Profile].Displayprefs) == 0 {
				app.err.Printf("No homescreen saved in profile \"%s\"", req.Profile)
				respond(500, "No homescreen template available", gc)
				return
			}
			configuration = app.storage.profiles[req.Profile].Configuration
			displayprefs = app.storage.profiles[req.Profile].Displayprefs
		}
		policy = app.storage.profiles[req.Profile].Policy
	} else if req.From == "user" {
		applyingFrom = "user"
		user, status, err := app.jf.UserByID(req.ID, false)
		if !(status == 200 || status == 204) || err != nil {
			app.err.Printf("Failed to get user from Jellyfin: Code %d", status)
			app.debug.Printf("Error: %s", err)
			respond(500, "Couldn't get user", gc)
			return
		}
		applyingFrom = "\"" + user["Name"].(string) + "\""
		policy = user["Policy"].(map[string]interface{})
		if req.Homescreen {
			displayprefs, status, err = app.jf.GetDisplayPreferences(req.ID)
			if !(status == 200 || status == 204) || err != nil {
				app.err.Printf("Failed to get DisplayPrefs: Code %d", status)
				app.debug.Printf("Error: %s", err)
				respond(500, "Couldn't get displayprefs", gc)
				return
			}
			configuration = user["Configuration"].(map[string]interface{})
		}
	}
	app.info.Printf("Applying settings to %d user(s) from %s", len(req.ApplyTo), applyingFrom)
	errors := errorListDTO{
		"policy":     map[string]string{},
		"homescreen": map[string]string{},
	}
	for _, id := range req.ApplyTo {
		status, err := app.jf.SetPolicy(id, policy)
		if !(status == 200 || status == 204) || err != nil {
			errors["policy"][id] = fmt.Sprintf("%d: %s", status, err)
		}
		if req.Homescreen {
			status, err = app.jf.SetConfiguration(id, configuration)
			errorString := ""
			if !(status == 200 || status == 204) || err != nil {
				errorString += fmt.Sprintf("Configuration %d: %s ", status, err)
			} else {
				status, err = app.jf.SetDisplayPreferences(id, displayprefs)
				if !(status == 200 || status == 204) || err != nil {
					errorString += fmt.Sprintf("Displayprefs %d: %s ", status, err)
				}
			}
			if errorString != "" {
				errors["homescreen"][id] = errorString
			}
		}
	}
	code := 200
	if len(errors["policy"]) == len(req.ApplyTo) || len(errors["homescreen"]) == len(req.ApplyTo) {
		code = 500
	}
	gc.JSON(code, errors)
}

// @Summary Get jfa-go configuration.
// @Produce json
// @Success 200 {object} settings "Uses the same format as config-base.json"
// @Router /config [get]
// @Security Bearer
// @tags Configuration
func (app *appContext) GetConfig(gc *gin.Context) {
	app.info.Println("Config requested")
	resp := app.configBase
	// Load language options
	formOptions := app.storage.lang.Form.getOptions()
	fl := resp.Sections["ui"].Settings["language-form"]
	fl.Options = formOptions
	fl.Value = app.config.Section("ui").Key("language-form").MustString("en-us")
	adminOptions := app.storage.lang.Admin.getOptions()
	al := resp.Sections["ui"].Settings["language-admin"]
	al.Options = adminOptions
	al.Value = app.config.Section("ui").Key("language-admin").MustString("en-us")
	emailOptions := app.storage.lang.Email.getOptions()
	el := resp.Sections["email"].Settings["language"]
	el.Options = emailOptions
	el.Value = app.config.Section("email").Key("language").MustString("en-us")
	for sectName, section := range resp.Sections {
		for settingName, setting := range section.Settings {
			val := app.config.Section(sectName).Key(settingName)
			s := resp.Sections[sectName].Settings[settingName]
			switch setting.Type {
			case "text", "email", "select", "password":
				s.Value = val.MustString("")
			case "number":
				s.Value = val.MustInt(0)
			case "bool":
				s.Value = val.MustBool(false)
			}
			resp.Sections[sectName].Settings[settingName] = s
		}
	}
	resp.Sections["ui"].Settings["language-form"] = fl
	resp.Sections["ui"].Settings["language-admin"] = al
	resp.Sections["email"].Settings["language"] = el

	gc.JSON(200, resp)
}

// @Summary Modify app config.
// @Produce json
// @Param appConfig body configDTO true "Config split into sections as in config.ini, all values as strings."
// @Success 200 {object} boolResponse
// @Router /config [post]
// @Security Bearer
// @tags Configuration
func (app *appContext) ModifyConfig(gc *gin.Context) {
	app.info.Println("Config modification requested")
	var req configDTO
	gc.BindJSON(&req)
	tempConfig, _ := ini.Load(app.configPath)
	for section, settings := range req {
		if section != "restart-program" {
			_, err := tempConfig.GetSection(section)
			if err != nil {
				tempConfig.NewSection(section)
			}
			for setting, value := range settings.(map[string]interface{}) {
				if value.(string) != app.config.Section(section).Key(setting).MustString("") {
					tempConfig.Section(section).Key(setting).SetValue(value.(string))
				}
			}
		}
	}
	tempConfig.SaveTo(app.configPath)
	app.debug.Println("Config saved")
	gc.JSON(200, map[string]bool{"success": true})
	if req["restart-program"] != nil && req["restart-program"].(bool) {
		app.info.Println("Restarting...")
		err := app.Restart()
		if err != nil {
			app.err.Printf("Couldn't restart, try restarting manually. (%s)", err)
		}
	}
	app.loadConfig()
	// Reinitialize password validator on config change, as opposed to every applicable request like in python.
	if _, ok := req["password_validation"]; ok {
		app.debug.Println("Reinitializing validator")
		validatorConf := ValidatorConf{
			"length":    app.config.Section("password_validation").Key("min_length").MustInt(0),
			"uppercase": app.config.Section("password_validation").Key("upper").MustInt(0),
			"lowercase": app.config.Section("password_validation").Key("lower").MustInt(0),
			"number":    app.config.Section("password_validation").Key("number").MustInt(0),
			"special":   app.config.Section("password_validation").Key("special").MustInt(0),
		}
		if !app.config.Section("password_validation").Key("enabled").MustBool(false) {
			for key := range validatorConf {
				validatorConf[key] = 0
			}
		}
		app.validator.init(validatorConf)
	}
}

// @Summary Logout by deleting refresh token from cookies.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /logout [post]
// @tags Other
func (app *appContext) Logout(gc *gin.Context) {
	cookie, err := gc.Cookie("refresh")
	if err != nil {
		app.debug.Printf("Couldn't get cookies: %s", err)
		respond(500, "Couldn't fetch cookies", gc)
		return
	}
	app.invalidTokens = append(app.invalidTokens, cookie)
	gc.SetCookie("refresh", "invalid", -1, "/", gc.Request.URL.Hostname(), true, true)
	respondBool(200, true, gc)
}

// @Summary Returns a map of available language codes to their full names, usable in the lang query parameter.
// @Produce json
// @Success 200 {object} langDTO
// @Failure 500 {object} stringResponse
// @Router /lang [get]
// @tags Other
func (app *appContext) GetLanguages(gc *gin.Context) {
	page := gc.Param("page")
	resp := langDTO{}
	if page == "form" {
		for key, lang := range app.storage.lang.Form {
			resp[key] = lang.Meta.Name
		}
	} else if page == "admin" {
		for key, lang := range app.storage.lang.Admin {
			resp[key] = lang.Meta.Name
		}
	} else if page == "setup" {
		for key, lang := range app.storage.lang.Setup {
			resp[key] = lang.Meta.Name
		}
	} else if page == "email" {
		for key, lang := range app.storage.lang.Email {
			resp[key] = lang.Meta.Name
		}
	}
	if len(resp) == 0 {
		respond(500, "Couldn't get languages", gc)
		return
	}
	gc.JSON(200, resp)
}

func (app *appContext) restart(gc *gin.Context) {
	app.info.Println("Restarting...")
	err := app.Restart()
	if err != nil {
		app.err.Printf("Couldn't restart, try restarting manually. (%s)", err)
	}
}

func (app *appContext) ServeLang(gc *gin.Context) {
	page := gc.Param("page")
	lang := strings.Replace(gc.Param("file"), ".json", "", 1)
	if page == "admin" {
		gc.JSON(200, app.storage.lang.Admin[lang])
		return
	} else if page == "form" {
		gc.JSON(200, app.storage.lang.Form[lang])
		return
	}
	respondBool(400, false, gc)
}

// no need to syscall.exec anymore!
func (app *appContext) Restart() error {
	RESTART <- true
	return nil
}
