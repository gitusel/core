package emailutil

import (
	"fmt"

	"windshift/internal/i18n"
	"windshift/internal/models"
)

// TemplateResolver supplies localized admin overrides.
// A nil resolver uses embedded defaults only.
type TemplateResolver interface {
	ResolveForLocale(name, locale string) (*models.EmailTemplate, bool, error)
}

// Renderer renders named templates and notification digests.
type Renderer struct {
	Templates TemplateResolver
	BaseURL   string
}

// Render renders a named template with the default locale.
func (r *Renderer) Render(templateName string, data any) (subject, htmlBody, textBody string, err error) {
	return r.RenderLocale(i18n.Default, templateName, data)
}

// RenderLocale renders a named template in the given locale.
func (r *Renderer) RenderLocale(locale, templateName string, data any) (subject, htmlBody, textBody string, err error) {
	resolver := func(loc, name string) (TemplateSource, error) {
		if r.Templates != nil {
			if t, fromDB, err := r.Templates.ResolveForLocale(name, loc); err == nil && t != nil {
				return TemplateSource{Subject: t.Subject, HTMLBody: t.HTMLBody, TextBody: t.TextBody, FromDB: fromDB}, nil
			}
		}
		if t, ok := LookupDefaultTemplate(name); ok {
			return TemplateSource{Subject: t.Subject, HTMLBody: t.HTMLBody, TextBody: t.TextBody, FromDB: false}, nil
		}
		return TemplateSource{}, fmt.Errorf("unknown template %q", name)
	}
	return RenderNamed(locale, templateName, resolver, data)
}

// RenderBatch renders the notification digest.
func (r *Renderer) RenderBatch(locale, timezone, userName string, notifications []models.Notification) (subject, htmlBody, textBody string, err error) {
	return r.RenderLocale(locale, TemplateNotificationBatch, BuildNotificationBatchData(r.BaseURL, userName, locale, timezone, notifications))
}
