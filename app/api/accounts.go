package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/mariusor/littr.go/app"

	"github.com/mariusor/littr.go/app/frontend"

	"github.com/go-chi/chi"
	"github.com/juju/errors"
	ap "github.com/mariusor/activitypub.go/activitypub"
	as "github.com/mariusor/activitypub.go/activitystreams"
	json "github.com/mariusor/activitypub.go/jsonld"
	"github.com/mariusor/littr.go/app/models"
	log "github.com/sirupsen/logrus"
)

func getObjectID(s string) as.ObjectID {
	return as.ObjectID(fmt.Sprintf("%s/%s", AccountsURL, s))
}

func apAccountID(a models.Account) as.ObjectID {
	if len(a.Hash) >= 8 {
		return as.ObjectID(fmt.Sprintf("%s/%s", AccountsURL, a.Hash.String()))
	}
	return as.ObjectID(fmt.Sprintf("%s/anonymous", AccountsURL))
}

func loadAPLike(vote models.Vote) as.ObjectOrLink {
	if vote.Weight == 0 {
		return nil
	}
	id, _ := BuildObjectIDFromItem(*vote.Item)
	lID := BuildObjectIDFromVote(vote)
	whomArt := as.IRI(BuildActorHashID(*vote.SubmittedBy))
	if vote.Weight > 0 {
		l := as.LikeNew(lID, as.IRI(id))
		l.AttributedTo = whomArt
		return *l
	} else {
		l := as.DislikeNew(lID, as.IRI(id))
		l.AttributedTo = whomArt
		return *l
	}
}

func loadAPItem(item models.Item) as.Item {
	o := Article{}
	o.Name = make(as.NaturalLanguageValue, 0)
	o.Content = make(as.NaturalLanguageValue, 0)
	if id, ok := BuildObjectIDFromItem(item); ok {
		o.ID = id
	}
	o.URL = as.IRI(frontend.ItemPermaLink(item))
	if item.MimeType == models.MimeTypeURL {
		o.Type = as.PageType
	} else {
		o.Type = as.NoteType
	}
	o.Published = item.SubmittedAt
	o.Updated = item.UpdatedAt
	o.MediaType = as.MimeType(item.MimeType)
	o.Generator = as.IRI(app.Instance.BaseURL)
	o.Score = item.Score / models.ScoreMultiplier
	if item.Title != "" {
		o.Name.Set("en", string(item.Title))
	}
	if item.Data != "" {
		o.Content.Set("en", string(item.Data))
	}
	if item.SubmittedBy != nil {
		o.AttributedTo = as.IRI(BuildActorID(*item.SubmittedBy))
	}
	if item.Parent != nil {
		id, _ := BuildObjectIDFromItem(*item.Parent)
		o.InReplyTo = as.IRI(id)
	}
	if item.OP != nil {
		id, _ := BuildObjectIDFromItem(*item.OP)
		o.Context = as.IRI(id)
	}
	return o
}

func loadAPPerson(a models.Account) *Person {
	p := Person{}
	p.Type = as.PersonType
	p.Name = as.NaturalLanguageValueNew()
	p.PreferredUsername = as.NaturalLanguageValueNew()

	if len(a.Hash) >= 8 {
		p.ID = apAccountID(a)
	}

	p.Name.Set("en", a.Handle)
	p.PreferredUsername.Set("en", a.Handle)

	out := ap.OutboxNew()
	out.ID = BuildCollectionID(a, new(ap.Outbox))
	p.Outbox = out

	in := ap.InboxNew()
	in.ID = BuildCollectionID(a, new(ap.Inbox))
	p.Inbox = in

	liked := ap.LikedNew()
	liked.ID = BuildCollectionID(a, new(ap.Liked))
	p.Liked = liked

	p.URL = as.IRI(frontend.AccountPermaLink(a))
	p.Score = a.Score
	if a.IsValid() && a.HasMetadata() && a.Metadata.Key != nil && a.Metadata.Key.Public != nil {
		p.PublicKey = PublicKey{
			ID:           as.ObjectID(fmt.Sprintf("%s#main-key", p.ID)),
			Owner:        as.IRI(p.ID),
			PublicKeyPem: fmt.Sprintf("-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----", base64.StdEncoding.EncodeToString(a.Metadata.Key.Public)),
		}
	}
	return &p
}

func loadAPLiked(o as.CollectionInterface, votes models.VoteCollection) (as.CollectionInterface, error) {
	if votes == nil || len(votes) == 0 {
		return nil, errors.Errorf("empty collection %T", o)
	}
	for _, vote := range votes {
		el := loadAPLike(vote)

		if el != nil {
			o.Append(el)
		}
	}

	return o, nil
}

func loadAPCollection(o as.CollectionInterface, items *models.ItemCollection) (as.CollectionInterface, error) {
	if items == nil || len(*items) == 0 {
		return nil, errors.Errorf("empty collection %T", o)
	}
	for _, item := range *items {
		o.Append(loadAPItem(item))
	}

	return o, nil
}

// GET /api/accounts?filters
func HandleAccountsCollection(w http.ResponseWriter, r *http.Request) {
	var ok bool
	var filter models.LoadAccountsFilter
	var data []byte

	f := r.Context().Value(models.FilterCtxtKey)
	if filter, ok = f.(models.LoadAccountsFilter); !ok {
		Logger.WithFields(log.Fields{}).Errorf("could not load filter from Context")
		return
	} else {
		val := r.Context().Value(models.RepositoryCtxtKey)
		if service, ok := val.(models.CanLoadAccounts); ok {
			var accounts models.AccountCollection
			var err error

			col := as.CollectionNew(as.ObjectID(AccountsURL))
			if accounts, err = service.LoadAccounts(filter); err == nil {
				for _, acct := range accounts {
					p := loadAPPerson(acct)
					p.Outbox = as.IRI(BuildCollectionID(acct, p.Outbox))
					p.Liked = as.IRI(BuildCollectionID(acct, p.Liked))
					col.Append(p)
				}
				if len(accounts) > 0 {
					fpUrl := string(*col.GetID()) + "?page=1"
					col.First = as.IRI(fpUrl)
				}
				data, err = json.WithContext(GetContext()).Marshal(col)
			} else {
				Logger.WithFields(log.Fields{"trace": errors.Trace(err)}).Error(err)
			}
		}
	}
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	//w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /api/accounts/:handle
func HandleAccount(w http.ResponseWriter, r *http.Request) {
	val := r.Context().Value(AccountCtxtKey)
	a, _ := val.(models.Account)
	//if !ok {
	//	Logger.WithFields(log.Fields{}).Errorf("could not load Account from Context")
	//}
	p := loadAPPerson(a)
	if p.Outbox != nil {
		p.Outbox = as.IRI(*p.Outbox.GetID())
	}
	if p.Liked != nil {
		p.Liked = as.IRI(*p.Liked.GetID())
	}
	if p.Inbox != nil {
		p.Inbox = as.IRI(*p.Inbox.GetID())
	}
	p.Endpoints = as.Endpoints{SharedInbox: as.IRI(fmt.Sprintf("%s/api/inbox", app.Instance.BaseURL))}

	j, err := json.WithContext(GetContext()).Marshal(p)
	if err != nil {
		Logger.WithFields(log.Fields{"trace": errors.Trace(err)}).Error(err)
		HandleError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Del("Cookie")
	w.Header().Set("Content-Type", "application/activity+json")
	//w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(j)
}

// GET /api/accounts/:handle/:collection/:hash
// GET /api/:collection/:hash
func HandleCollectionItem(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error

	val := r.Context().Value(ItemCtxtKey)
	collection := chi.URLParam(r, "collection")
	var el as.ObjectOrLink
	switch strings.ToLower(collection) {
	case "inbox":
	case "outbox":
		i, ok := val.(models.Item)
		if !ok {
			Logger.WithFields(log.Fields{}).Errorf("could not load Item from Context")
			HandleError(w, r, http.StatusInternalServerError, err)
			return
		}
		el = loadAPItem(i)
		if err != nil {
			HandleError(w, r, http.StatusNotFound, err)
			return
		}
		val := r.Context().Value(models.RepositoryCtxtKey)
		if service, ok := val.(models.CanLoadItems); ok && len(i.Hash) > 0 {
			replies, err := service.LoadItems(models.LoadItemsFilter{
				InReplyTo: []string{i.Hash.String()},
				MaxItems:  MaxContentItems,
			})
			if err != nil {
				Logger.WithFields(log.Fields{"trace": errors.Trace(err)}).Error(err)
			}
			if len(replies) > 0 {
				if o, ok := el.(Article); ok {
					o.Replies = as.IRI(BuildRepliesCollectionID(o))
					el = o
				}
			}
		}
	case "liked":
		if v, ok := val.(models.Vote); !ok {
			Logger.WithFields(log.Fields{}).Errorf("could not load Vote from Context")
		} else {
			el = loadAPLike(v)
		}
	default:
		Logger.WithFields(log.Fields{}).Error(errors.Errorf("collection not found"))
	}

	data, err = json.WithContext(GetContext()).Marshal(el)
	w.Header().Del("Cookie")
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	//w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /api/accounts/:handle/:collection/:hash/replies
// GET /api/:collection/:hash/replies
func HandleItemReplies(w http.ResponseWriter, r *http.Request) {
	var ok bool

	var it models.Item
	item := r.Context().Value(ItemCtxtKey)
	var data []byte
	if it, ok = item.(models.Item); !ok {
		Logger.WithFields(log.Fields{}).Errorf("could not load Item from Context")
		return
	} else {
		val := r.Context().Value(models.RepositoryCtxtKey)
		el := loadAPItem(it)
		if service, ok := val.(models.CanLoadItems); ok {
			filter := models.LoadItemsFilter{
				InReplyTo: []string{it.Hash.String()},
				MaxItems:  MaxContentItems,
			}

			p, _ := el.(Article)
			col := as.CollectionNew(BuildRepliesCollectionID(p))
			if replies, err := service.LoadItems(filter); err == nil {
				_, err = loadAPCollection(col, &replies)
				data, err = json.WithContext(GetContext()).Marshal(p.Replies)
			} else {
				Logger.WithFields(log.Fields{}).Error(err)
			}
			p.Replies = col
		}
	}

	w.Header().Del("Cookie")
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	//w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /api/accounts/:handle/:collection
// GET /api/:collection
func HandleCollection(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error
	val := r.Context().Value(AccountCtxtKey)
	a, _ := val.(models.Account)
	//if !ok {
	//	Logger.WithFields(log.Fields{}).WithFields(log.Fields{}).Errorf("could not load Account from Context")
	//}
	p := loadAPPerson(a)

	typ := chi.URLParam(r, "collection")

	collection := r.Context().Value(CollectionCtxtKey)
	switch strings.ToLower(typ) {
	case "inbox":
	case "outbox":
		items, ok := collection.(models.ItemCollection)
		if !ok {
			Logger.WithFields(log.Fields{}).Errorf("could not load Items from Context")
			return
		}
		col := ap.OutboxNew()
		col.ID = BuildCollectionID(a, col)
		_, err = loadAPCollection(col, &items)
		if len(items) > 0 {
			fpUrl := string(*col.GetID()) + "?page=1"
			col.First = as.IRI(fpUrl)
		}
		p.Outbox = col
		data, err = json.WithContext(GetContext()).Marshal(p.Outbox)
	case "liked":
		votes, ok := collection.(models.VoteCollection)
		if !ok {
			Logger.WithFields(log.Fields{}).Errorf("could not load Votes from Context")
			return
		}
		if err != nil {
			Logger.WithFields(log.Fields{}).Error(err)
		}
		liked := ap.LikedNew()
		liked.ID = BuildCollectionID(a, liked)
		_, err = loadAPLiked(liked, votes)
		if len(votes) > 0 {
			fpUrl := string(*liked.GetID()) + "?page=1"
			liked.First = as.IRI(fpUrl)
		}
		p.Liked = liked
		data, err = json.WithContext(GetContext()).Marshal(p.Liked)
	case "replies":
		items, ok := collection.(models.ItemCollection)
		if !ok {
			log.Errorf("could not load Replies from Context")
			return
		}
		replies := OrderedCollectionNew(as.ObjectID(""))
		replies.ID = BuildCollectionID(a, replies)
		_, err = loadAPCollection(replies, &items)
		p.Replies = replies
		if len(items) > 0 {
			fpUrl := string(*replies.GetID()) + "?page=1"
			replies.First = as.IRI(fpUrl)
		}
		data, err = json.WithContext(GetContext()).Marshal(p.Outbox)
	default:
		err = errors.Errorf("collection not found")
	}

	if err != nil {
		HandleError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Del("Cookie")
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	//w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /api/accounts/verify_credentials
func HandleVerifyCredentials(w http.ResponseWriter, r *http.Request) {
	acct, ok := models.ContextCurrentAccount(r.Context())
	if !ok {
		HandleError(w, r, http.StatusNotFound, errors.Errorf("account not found"))
		return
	}
	AcctLoader, ok := models.ContextAccountLoader(r.Context())
	if !ok {
		Logger.WithFields(log.Fields{}).Errorf("could not load account repository from Context")
	}
	a, err := AcctLoader.LoadAccount(models.LoadAccountsFilter{Handle: []string{acct.Handle}, MaxItems: 1})
	if err != nil {
		Logger.WithFields(log.Fields{}).Error(err)
		HandleError(w, r, http.StatusNotFound, err)
		return
	}
	if a.Handle == "" {
		HandleError(w, r, http.StatusNotFound, errors.Errorf("account not found"))
		return
	}

	p := loadAPPerson(a)

	j, err := json.WithContext(GetContext()).Marshal(p)
	if err != nil {
		HandleError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Del("Cookie")
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	//w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(j)
}
