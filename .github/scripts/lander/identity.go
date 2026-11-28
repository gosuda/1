package lander

import (
	"context"
	"fmt"
	"net/url"
)

// BotIdentity returns the commit identity GitHub associates with the App: the name
// "<slug>[bot]" and the email "<bot user id>+<slug>[bot]@users.noreply.github.com". Commits with
// that email are attributed to the App on GitHub and show the App's logo as their avatar. Note
// that the id in the email is the id of the bot user, not the App id.
func (a *App) BotIdentity(ctx context.Context) (Identity, error) {
	jwt, err := a.jwt()
	if err != nil {
		return Identity{}, err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := a.call(ctx, "GET", "/app", jwt, nil, &app); err != nil {
		return Identity{}, fmt.Errorf("read app: %w", err)
	}
	if app.Slug == "" {
		return Identity{}, fmt.Errorf("read app: response has no slug")
	}
	login := app.Slug + "[bot]"
	var user struct {
		ID int64 `json:"id"`
	}
	// Public endpoint: an App JWT is rejected outside /app routes, so no credentials are sent.
	if err := a.call(ctx, "GET", "/users/"+url.PathEscape(login), "", nil, &user); err != nil {
		return Identity{}, fmt.Errorf("read bot user %s: %w", login, err)
	}
	if user.ID == 0 {
		return Identity{}, fmt.Errorf("read bot user %s: response has no id", login)
	}
	return Identity{Name: login, Email: fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, login)}, nil
}
