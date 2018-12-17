package frontend

import (
	"bytes"
	"fmt"
	"github.com/go-chi/chi"
	"github.com/mariusor/littr.go/app"
	"github.com/mariusor/littr.go/app/log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/juju/errors"
)

func detectMimeType(data string) string {
	u, err := url.ParseRequestURI(data)
	if err == nil && u != nil && !bytes.ContainsRune([]byte(data), '\n') {
		return app.MimeTypeURL
	}
	return "text/plain"
}

func loadTags(data string) (app.TagCollection, app.TagCollection) {
	if !strings.ContainsAny(data, "#@~") {
		return nil, nil
	}
	tags := make(app.TagCollection, 0)
	mentions := make(app.TagCollection, 0)
	l := len(data)
	for i := 0; i < l; i++ {
		byt := data[i:]
		st := strings.IndexAny(byt, "#@~")
		if st >= l {
			break
		}
		if st != -1 {
			t := app.Tag{}
			en := strings.IndexAny(byt[st:], " \t\r\n,.:;!?")
			if en == -1 {
				en = len(byt) - st
			}
			if en == 1 {
				continue
			}
			t.Name = byt[st : st+en]
			if t.Name[0] == '#' {
				// @todo(marius) :link_generation: make the tag links be generated from the corresponding route
				t.URL = fmt.Sprintf("%s/t/%s", app.Instance.BaseURL, t.Name[1:])
				tags = append(tags, t)
			}
			if t.Name[0] == '@' || t.Name[0] == '~' {
				t.URL = fmt.Sprintf("%s/~%s", app.Instance.BaseURL, t.Name[1:])
				mentions = append(mentions, t)
			}

			i = i + st + en
		}
	}

	return tags, mentions
}

func ContentFromRequest(r *http.Request, acc app.Account) (app.Item, error) {
	if r.Method != http.MethodPost {
		return app.Item{}, errors.Errorf("invalid http method type")
	}

	i := app.Item{}
	tit := r.PostFormValue("title")
	if len(tit) > 0 {
		i.Title = tit
	}
	dat := r.PostFormValue("data")
	if len(dat) > 0 {
		i.Data = dat
	}

	i.SubmittedBy = &acc
	i.MimeType = detectMimeType(i.Data)
	i.Metadata = &app.ItemMetadata{}
	i.Metadata.Tags, i.Metadata.Mentions = loadTags(i.Data)
	if !i.IsLink() {
		i.MimeType = r.PostFormValue("mime-type")
	}
	if len(i.Data) > 0 {
		now := time.Now()
		i.SubmittedAt = now
		i.UpdatedAt = now
	}
	parent := r.PostFormValue("parent")
	i.Parent = &app.Item{Hash: app.Hash(parent)}
	return i, nil
}

// ShowSubmit serves GET /submit request
func (h *handler) ShowSubmit(w http.ResponseWriter, r *http.Request) {
	h.RenderTemplate(r, w, "new", contentModel{Title: "New submission"})
}

func (h *handler) ValidatePermissions(actions ...string) func (http.Handler) http.Handler {
	if len(actions) == 0 {
		return h.ValidateItemAuthor
	}
	// @todo(marius): implement permission logic
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		}
		return http.HandlerFunc(fn)
	}
}

func (h *handler) ValidateItemAuthor(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		acc := h.account
		hash := chi.URLParam(r, "hash")
		url := r.URL
		action := path.Base(url.Path)
		if len(hash) > 0 && action != hash {
			val := r.Context().Value(app.RepositoryCtxtKey)
			itemLoader, ok := val.(app.CanLoadItems)
			if !ok {
				h.logger.Error("could not load item repository from Context")
				return
			}
			m, err := itemLoader.LoadItem(app.LoadItemsFilter{Key: []string{hash}})
			if err != nil {
				h.logger.Error(err.Error())
				h.HandleError(w, r, errors.NewNotFound(err, "item"))
				return
			}
			if !sameHash(m.SubmittedBy.Hash, acc.Hash) {
				url.Path = path.Dir(url.Path)
				h.Redirect(w, r, url.RequestURI(), http.StatusTemporaryRedirect)
				return
			}
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}

// HandleSubmit handles POST /submit requests
// HandleSubmit handles POST /~handler/hash requests
// HandleSubmit handles POST /year/month/day/hash requests
// HandleSubmit handles POST /~handler/hash/edit requests
// HandleSubmit handles POST /year/month/day/hash/edit requests
func (h *handler) HandleSubmit(w http.ResponseWriter, r *http.Request) {
	p, err := ContentFromRequest(r, h.account)
	if err != nil {
		h.logger.WithContext(log.Ctx{
			"prev": err,
		}).Error("wrong http method")
		h.HandleError(w, r, errors.NewMethodNotAllowed(err, ""))
		return
	}

	acc := h.account
	auth, authOk := app.ContextAuthenticated(r.Context())
	if authOk && acc.IsLogged() {
		auth.WithAccount(&acc)
	}

	if repo, ok := app.ContextItemSaver(r.Context()); !ok {
		h.logger.Error("could not load item repository from Context")
		return
	} else {
		p, err = repo.SaveItem(p)
		if err != nil {
			h.logger.WithContext(log.Ctx{
				"prev": err,
			}).Error("unable to save item")
			h.HandleError(w, r, errors.NewNotValid(err, "oops!"))
			return
		}
	}
	if voter, ok := app.ContextVoteSaver(r.Context()); !ok {
		h.logger.Error("could not load item repository from Context")
	} else {
		v := app.Vote{
			SubmittedBy: &acc,
			Item:        &p,
			Weight:      1 * app.ScoreMultiplier,
		}
		if _, err := voter.SaveVote(v); err != nil {
			h.logger.WithContext(log.Ctx{
				"hash":   v.Item.Hash,
				"author": v.SubmittedBy.Handle,
				"weight": v.Weight,
			}).Error(err.Error())
		}
	}
	h.Redirect(w, r, ItemPermaLink(p), http.StatusSeeOther)
}
