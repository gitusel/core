// Package-level email rendering helpers shared by every outbound provider.
//
// These helpers were extracted from the SMTP sender so that the Graph
// provider (and any future provider) renders byte-identical digests and
// transactional mail without importing the SMTP package. Nothing here knows
// which transport will deliver the message.
package emailutil

import (
	"log/slog"
	"strings"

	"windshift/internal/i18n"
	"windshift/internal/models"
)

// TemplateSource contains the subject and body templates. FromDB marks an
// admin override so render failures can fall back to the embedded default.
type TemplateSource struct {
	Subject, HTMLBody, TextBody string
	FromDB                      bool
}

// ResolveTemplateFunc returns the template sources for (locale, name).
type ResolveTemplateFunc func(locale, name string) (TemplateSource, error)

// RenderNamed renders a named template, falling back to the embedded default
// if an admin override fails to render.
func RenderNamed(locale, templateName string, resolve ResolveTemplateFunc, data any) (subject, htmlBody, textBody string, err error) {
	src, err := resolve(locale, templateName)
	if err != nil {
		return "", "", "", err
	}
	subject, htmlBody, textBody, err = renderEmailTemplates(locale, src.Subject, src.HTMLBody, src.TextBody, data)
	if err == nil {
		return subject, htmlBody, textBody, nil
	}
	if !src.FromDB {
		return "", "", "", err
	}
	def, ok := LookupDefaultTemplate(templateName)
	if !ok {
		return "", "", "", err
	}
	slog.Warn("admin email template failed to render; using built-in default",
		slog.String("template", templateName),
		slog.Any("error", err),
	)
	return renderEmailTemplates(locale, def.Subject, def.HTMLBody, def.TextBody, data)
}

func renderEmailTemplates(locale, subjectSrc, htmlSrc, textSrc string, data any) (subject, htmlBody, textBody string, err error) {
	_, subject, err = RenderTemplatesLocale(locale, subjectSrc, subjectSrc, data)
	if err != nil {
		return "", "", "", err
	}
	enriched := EnrichWithSubjectLocale(data, subject, locale)
	htmlBody, textBody, err = RenderTemplatesLocale(locale, htmlSrc, textSrc, enriched)
	if err != nil {
		return "", "", "", err
	}
	return subject, htmlBody, textBody, nil
}

func LookupDefaultTemplate(name string) (DefaultTemplate, bool) {
	for _, t := range DefaultTemplates() {
		if t.Name == name {
			return t, true
		}
	}
	return DefaultTemplate{}, false
}

// NotificationBatchEntry mirrors the per-row data exposed to the
// notification_batch template. AccentColor is a hex code used for the
// left-border accent, derived from the notification Type.
type NotificationBatchEntry struct {
	Title         string
	Message       string
	Type          string
	AccentColor   string
	FormattedTime string
	ActionURL     string
}

// BuildNotificationBatchData builds the template payload for the batch digest.
func BuildNotificationBatchData(baseURL, userName, locale, timezone string, notifications []models.Notification) any {
	location := i18n.Location(timezone)
	data := struct {
		UserName          string
		NotificationCount int
		Notifications     []NotificationBatchEntry
	}{
		UserName:          userName,
		NotificationCount: len(notifications),
		Notifications:     make([]NotificationBatchEntry, len(notifications)),
	}
	for i, n := range notifications {
		title, message := localizedNotificationCopy(locale, n)
		data.Notifications[i] = NotificationBatchEntry{
			Title:         title,
			Message:       message,
			Type:          n.Type,
			AccentColor:   NotificationAccentColor(n.Type),
			FormattedTime: i18n.FormatDateTimeZ(locale, n.Timestamp, location),
			ActionURL:     AbsoluteActionURL(baseURL, n.ActionURL),
		}
	}
	return data
}

func localizedNotificationCopy(locale string, n models.Notification) (title, message string) {
	title, message = n.Title, n.Message
	titleKey, messageKey, params, ok := models.ParseNotificationI18n(n.Metadata)
	if !ok {
		return title, message
	}
	if translated, found := i18n.TranslateOK(locale, titleKey, params); found {
		title = translated
	}
	if translated, found := i18n.TranslateOK(locale, messageKey, params); found {
		message = translated
	}
	return title, message
}

// AbsoluteActionURL turns a stored relative notification action path into a
// clickable absolute URL. Already-absolute URLs pass through, and an empty
// base URL leaves the path as-is so the template can still omit the link.
func AbsoluteActionURL(baseURL, actionURL string) string {
	actionURL = strings.TrimSpace(actionURL)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if actionURL == "" || baseURL == "" {
		return actionURL
	}
	if strings.HasPrefix(actionURL, "http://") || strings.HasPrefix(actionURL, "https://") || strings.HasPrefix(actionURL, "//") {
		return actionURL
	}
	if !strings.HasPrefix(actionURL, "/") {
		actionURL = "/" + actionURL
	}
	return baseURL + actionURL
}

// NotificationAccentColor maps notification types to a brand-aligned accent
// color used as the left border of each card in the email template.
func NotificationAccentColor(t string) string {
	switch t {
	case "success":
		return "#10b981"
	case "warning":
		return "#f59e0b"
	case "error":
		return "#ef4444"
	case "assignment":
		return "#8b5cf6"
	case "comment":
		return "#06b6d4"
	case "mention":
		return "#a855f7"
	case "status_change":
		return "#f97316"
	case "reminder":
		return "#84cc16"
	case "milestone":
		return "#ec4899"
	default:
		return "#2874bb"
	}
}
