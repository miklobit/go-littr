package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/client"
	"github.com/go-ap/errors"
	"github.com/go-ap/handlers"
	j "github.com/go-ap/jsonld"
	"github.com/mariusor/littr.go/internal/log"
	"github.com/mariusor/qstring"
	"github.com/spacemonkeygo/httpsig"
	"golang.org/x/sync/errgroup"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

var nilIRI = EqualsString("-")
var nilIRIs = CompStrs{nilIRI}

var notNilIRI = DifferentThanString("-")
var notNilIRIs = CompStrs{notNilIRI}

type repository struct {
	BaseURL string
	SelfURL string
	app     *Account
	fedbox  *fedbox
	infoFn  CtxLogFn
	errFn   CtxLogFn
}

var ValidActorTypes = pub.ActivityVocabularyTypes{
	pub.PersonType,
	pub.ServiceType,
	pub.GroupType,
	pub.ApplicationType,
	pub.OrganizationType,
}

var ValidItemTypes = pub.ActivityVocabularyTypes{
	pub.ArticleType,
	pub.NoteType,
	pub.LinkType,
	pub.PageType,
	pub.DocumentType,
	pub.VideoType,
	pub.AudioType,
}

// @deprecated
var ValidActivityTypes = pub.ActivityVocabularyTypes{
	pub.CreateType,
	pub.LikeType,
	pub.FollowType,
}

// Repository middleware
func (h handler) Repository(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), RepositoryCtxtKey, h.storage)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}

func ActivityPubService(c appConfig) *repository {
	pub.ItemTyperFunc = pub.JSONGetItemByType

	BaseURL = c.APIURL
	ActorsURL = actors.IRI(pub.IRI(BaseURL))
	ObjectsURL = objects.IRI(pub.IRI(BaseURL))

	infoFn := func(ctx ...log.Ctx) LogFn {
		return c.Logger.WithContext(append(ctx, log.Ctx{"client": "api"})...).Infof
	}
	errFn := func(ctx ...log.Ctx) LogFn {
		return c.Logger.WithContext(append(ctx, log.Ctx{"client": "api"})...).Errorf
	}
	ua := fmt.Sprintf("%s-%s", c.HostName, Instance.Version)

	f, _ := NewClient(SetURL(BaseURL), SetInfoLogger(infoFn), SetErrorLogger(errFn), SetUA(ua))

	return &repository{
		BaseURL: c.APIURL,
		SelfURL: c.BaseURL,
		fedbox:  f,
		infoFn:  infoFn,
		errFn:   errFn,
	}
}

func BuildCollectionID(a Account, o handlers.CollectionType) pub.ID {
	if len(a.Handle) > 0 {
		return pub.ID(fmt.Sprintf("%s/%s/%s", ActorsURL, url.PathEscape(a.Hash.String()), o))
	}
	return o.IRI(pub.IRI(BaseURL))
}

var BaseURL = "http://fedbox.git"
var ActorsURL = actors.IRI(pub.IRI(BaseURL))
var ObjectsURL = objects.IRI(pub.IRI(BaseURL))

func apAccountID(a Account) pub.ID {
	if len(a.Hash) >= 8 {
		return pub.ID(fmt.Sprintf("%s/%s", ActorsURL, a.Hash.String()))
	}
	return pub.ID(fmt.Sprintf("%s/anonymous", ActorsURL))
}

func accountURL(acc Account) pub.IRI {
	return pub.IRI(fmt.Sprintf("%s%s", Instance.BaseURL, AccountLocalLink(&acc)))
}

func BuildIDFromItem(i Item) (pub.ID, bool) {
	if !i.IsValid() {
		return "", false
	}
	if i.HasMetadata() && len(i.Metadata.ID) > 0 {
		return pub.ID(i.Metadata.ID), true
	}
	return pub.ID(fmt.Sprintf("%s/%s", ObjectsURL, url.PathEscape(i.Hash.String()))), true
}

func BuildActorID(a Account) pub.ID {
	if !a.IsValid() {
		return pub.ID(pub.PublicNS)
	}
	if a.HasMetadata() && len(a.Metadata.ID) > 0 {
		return pub.ID(a.Metadata.ID)
	}
	return pub.ID(fmt.Sprintf("%s/%s", ActorsURL, url.PathEscape(a.Hash.String())))
}

func loadAPItem(it pub.Item, item Item) error {
	return pub.OnObject(it, func(o *pub.Object) error {
		if id, ok := BuildIDFromItem(item); ok {
			o.ID = id
		}
		if item.MimeType == MimeTypeURL {
			o.Type = pub.PageType
			o.URL = pub.IRI(item.Data)
		} else {
			wordCount := strings.Count(item.Data, " ") +
				strings.Count(item.Data, "\t") +
				strings.Count(item.Data, "\n") +
				strings.Count(item.Data, "\r\n")
			if wordCount > 300 {
				o.Type = pub.ArticleType
			} else {
				o.Type = pub.NoteType
			}

			if len(item.Hash) > 0 {
				o.URL = pub.IRI(ItemPermaLink(&item))
			}
			o.Name = make(pub.NaturalLanguageValues, 0)
			switch item.MimeType {
			case MimeTypeMarkdown:
				o.Source.MediaType = pub.MimeType(item.MimeType)
				o.MediaType = MimeTypeHTML
				if item.Data != "" {
					o.Source.Content.Set("en", pub.Content(item.Data))
					o.Content.Set("en", pub.Content(Markdown(item.Data)))
				}
			case MimeTypeText:
				fallthrough
			case MimeTypeHTML:
				o.MediaType = pub.MimeType(item.MimeType)
				o.Content.Set("en", pub.Content(item.Data))
			}
		}

		o.Published = item.SubmittedAt
		o.Updated = item.UpdatedAt

		if item.Deleted() {
			del := pub.Tombstone{
				ID:         o.ID,
				Type:       pub.TombstoneType,
				FormerType: o.Type,
				Deleted:    o.Updated,
			}
			repl := make(pub.ItemCollection, 0)
			if item.Parent != nil {
				if par, ok := BuildIDFromItem(*item.Parent); ok {
					repl = append(repl, par)
				}
				if item.OP == nil {
					item.OP = item.Parent
				}
			}
			if item.OP != nil {
				if op, ok := BuildIDFromItem(*item.OP); ok {
					del.Context = op
					if !repl.Contains(op) {
						repl = append(repl, op)
					}
				}
			}
			if len(repl) > 0 {
				del.InReplyTo = repl
			}

			it = &del
			return nil
		}

		if item.Title != "" {
			o.Name.Set("en", pub.Content(item.Title))
		}
		if item.SubmittedBy != nil {
			o.AttributedTo = BuildActorID(*item.SubmittedBy)
		}

		to := make(pub.ItemCollection, 0)
		bcc := make(pub.ItemCollection, 0)
		cc := make(pub.ItemCollection, 0)
		repl := make(pub.ItemCollection, 0)

		if item.Parent != nil {
			p := item.Parent
			first := true
			for {
				if par, ok := BuildIDFromItem(*p); ok {
					repl = append(repl, par)
				}
				if p.SubmittedBy.IsValid() {
					if pAuth := BuildActorID(*p.SubmittedBy); !pub.PublicNS.Equals(pAuth, true) {
						if first {
							if !to.Contains(pAuth) {
								to = append(to, pAuth)
							}
							first = false
						} else if !cc.Contains(pAuth) {
							cc = append(cc, pAuth)
						}
					}
				}
				if p.Parent == nil {
					break
				}
				p = p.Parent
			}
		}
		if item.OP != nil {
			if op, ok := BuildIDFromItem(*item.OP); ok {
				o.Context = op
			}
		}
		if len(repl) > 0 {
			o.InReplyTo = repl
		}

		// TODO(marius): add proper dynamic recipients to this based on some selector in the frontend
		if !item.Private() {
			to = append(to, pub.PublicNS)
			bcc = append(bcc, pub.IRI(BaseURL))
		}
		if item.Metadata != nil {
			m := item.Metadata
			for _, rec := range m.To {
				mto := pub.IRI(rec.Metadata.ID)
				if !to.Contains(mto) {
					to = append(to, mto)
				}
			}
			for _, rec := range m.CC {
				mcc := pub.IRI(rec.Metadata.ID)
				if !cc.Contains(mcc) {
					cc = append(cc, mcc)
				}
			}
			if m.Mentions != nil || m.Tags != nil {
				o.Tag = make(pub.ItemCollection, 0)
				for _, men := range m.Mentions {
					// todo(marius): retrieve object ids of each mention and add it to the CC of the object
					t := pub.Object{
						ID:   pub.ID(men.URL),
						Type: pub.MentionType,
						Name: pub.NaturalLanguageValues{{Ref: pub.NilLangRef, Value: pub.Content(men.Name)}},
					}
					o.Tag.Append(t)
				}
				for _, tag := range m.Tags {
					t := pub.Object{
						ID:   pub.ID(tag.URL),
						Type: pub.ObjectType,
						Name: pub.NaturalLanguageValues{{Ref: pub.NilLangRef, Value: pub.Content(tag.Name)}},
					}
					o.Tag.Append(t)
				}
			}
		}
		o.To = to
		o.CC = cc
		o.BCC = bcc

		return nil
	})
}

var anonymousActor = &pub.Actor{
	ID:                pub.PublicNS,
	Name:              pub.NaturalLanguageValues{{pub.NilLangRef, pub.Content(Anonymous)}},
	Type:              pub.PersonType,
	PreferredUsername: pub.NaturalLanguageValues{{pub.NilLangRef, pub.Content(Anonymous)}},
}

func anonymousPerson(url string) *pub.Actor {
	p := anonymousActor
	p.Inbox = handlers.Inbox.IRI(pub.IRI(url))
	return p
}

func loadAPPerson(a Account) *pub.Actor {
	p := new(pub.Actor)
	p.Type = pub.PersonType
	p.Name = pub.NaturalLanguageValuesNew()
	p.PreferredUsername = pub.NaturalLanguageValuesNew()

	if a.HasMetadata() {
		if a.Metadata.Blurb != nil && len(a.Metadata.Blurb) > 0 {
			p.Summary = pub.NaturalLanguageValuesNew()
			p.Summary.Set(pub.NilLangRef, pub.Content(a.Metadata.Blurb))
		}
		if len(a.Metadata.Icon.URI) > 0 {
			avatar := pub.ObjectNew(pub.ImageType)
			avatar.MediaType = pub.MimeType(a.Metadata.Icon.MimeType)
			avatar.URL = pub.IRI(a.Metadata.Icon.URI)
			p.Icon = avatar
		}
	}

	p.PreferredUsername.Set(pub.NilLangRef, pub.Content(a.Handle))

	if len(a.Hash) > 0 {
		if a.IsFederated() {
			p.ID = pub.ID(a.Metadata.ID)
			p.Name.Set("en", pub.Content(a.Metadata.Name))
			if len(a.Metadata.InboxIRI) > 0 {
				p.Inbox = pub.IRI(a.Metadata.InboxIRI)
			}
			if len(a.Metadata.OutboxIRI) > 0 {
				p.Outbox = pub.IRI(a.Metadata.OutboxIRI)
			}
			if len(a.Metadata.LikedIRI) > 0 {
				p.Liked = pub.IRI(a.Metadata.LikedIRI)
			}
			if len(a.Metadata.FollowersIRI) > 0 {
				p.Followers = pub.IRI(a.Metadata.FollowersIRI)
			}
			if len(a.Metadata.FollowingIRI) > 0 {
				p.Following = pub.IRI(a.Metadata.FollowingIRI)
			}
			if len(a.Metadata.URL) > 0 {
				p.URL = pub.IRI(a.Metadata.URL)
			}
		} else {
			p.Name.Set("en", pub.Content(a.Handle))

			p.Outbox = BuildCollectionID(a, handlers.Outbox)
			p.Inbox = BuildCollectionID(a, handlers.Inbox)
			p.Liked = BuildCollectionID(a, handlers.Liked)

			p.URL = accountURL(a)

			if !a.CreatedAt.IsZero() {
				p.Published = a.CreatedAt
			}
			if !a.UpdatedAt.IsZero() {
				p.Updated = a.UpdatedAt
			}
		}
		if len(a.Hash) >= 8 {
			p.ID = apAccountID(a)
		}
		oauthURL := strings.Replace(BaseURL, "api", "oauth", 1)
		p.Endpoints = &pub.Endpoints{
			SharedInbox:                handlers.Inbox.IRI(pub.IRI(BaseURL)),
			OauthAuthorizationEndpoint: pub.IRI(fmt.Sprintf("%s/authorize", oauthURL)),
			OauthTokenEndpoint:         pub.IRI(fmt.Sprintf("%s/token", oauthURL)),
		}
	}

	//p.Score = a.Score
	if a.IsValid() && a.HasMetadata() && a.Metadata.Key != nil && a.Metadata.Key.Public != nil {
		p.PublicKey = pub.PublicKey{
			ID:           pub.ID(fmt.Sprintf("%s#main-key", p.ID)),
			Owner:        p.ID,
			PublicKeyPem: fmt.Sprintf("-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----", base64.StdEncoding.EncodeToString(a.Metadata.Key.Public)),
		}
	}
	return p
}

func getSigner(pubKeyID pub.ID, key crypto.PrivateKey) *httpsig.Signer {
	hdrs := []string{"(request-target)", "host", "date"}
	return httpsig.NewSigner(string(pubKeyID), key, httpsig.RSASHA256, hdrs)
}

// @todo(marius): the decision which sign function to use (the one for S2S or the one for C2S)
//   should be made in fedbox, because that's the place where we know if the request we're signing
//   is addressed to an IRI belonging to that specific fedbox instance or to another ActivityPub server
func (r *repository) WithAccount(a *Account) *repository {
	r.fedbox.SignFn(r.withAccountC2S(a))
	return r
}

func (r *repository) withAccountC2S(a *Account) client.RequestSignFn {
	return func(req *http.Request) error {
		// TODO(marius): this needs to be added to the federated requests, which we currently don't support
		if !a.IsValid() || !a.IsLogged() {
			return nil
		}
		if a.Metadata.OAuth.Token == nil {
			r.errFn(log.Ctx{
				"handle":   a.Handle,
				"logged":   a.IsLogged(),
				"metadata": a.Metadata,
			})("account has no OAuth2 token")
			return nil
		}
		a.Metadata.OAuth.Token.SetAuthHeader(req)
		return nil
	}
}

func (r *repository) withAccountS2S(a *Account) client.RequestSignFn {
	// TODO(marius): this needs to be added to the federated requests, which we currently don't support
	if !a.IsValid() || !a.IsLogged() {
		return nil
	}

	k := a.Metadata.Key
	if k == nil {
		return nil
	}
	var prv crypto.PrivateKey
	var err error
	if k.ID == "id-rsa" {
		prv, err = x509.ParsePKCS8PrivateKey(k.Private)
	}
	if err != nil {
		r.errFn(log.Ctx{
			"handle":   a.Handle,
			"logged":   a.IsLogged(),
			"metadata": a.Metadata,
		})(err.Error())
		return nil
	}
	if k.ID == "id-ecdsa" {
		//prv, err = x509.ParseECPrivateKey(k.Private)
		err := errors.Errorf("unsupported private key type %s", k.ID)
		r.errFn(log.Ctx{
			"handle":   a.Handle,
			"logged":   a.IsLogged(),
			"metadata": a.Metadata,
		})(err.Error())
		return nil
	}
	p := *loadAPPerson(*a)
	return getSigner(p.PublicKey.ID, prv).Sign
}

func (r *repository) LoadItem(ctx context.Context, iri pub.IRI) (Item, error) {
	var item Item
	art, err := r.fedbox.Object(ctx, iri)
	if err != nil {
		r.errFn()(err.Error())
		return item, err
	}
	if err = item.FromActivityPub(art); err == nil {
		var items ItemCollection
		items, err = r.loadItemsAuthors(ctx, item)
		items, err = r.loadItemsVotes(ctx, items...)
		if len(items) > 0 {
			item = items[0]
		}
	}
	return item, err
}

func hashesUnique(a Hashes) Hashes {
	u := make([]Hash, 0, len(a))
	m := make(map[string]bool)

	for _, val := range a {
		k := val.String()
		if _, ok := m[k]; !ok {
			m[k] = true
			u = append(u, val)
		}
	}
	return u
}

func (r *repository) loadAccountsVotes(ctx context.Context, accounts ...Account) (AccountCollection, error) {
	if len(accounts) == 0 {
		return accounts, nil
	}
	for _, account := range accounts {
		err := r.loadAccountVotes(ctx, &account, nil)
		if err != nil {
			r.errFn()(err.Error())
		}
	}
	return accounts, nil
}

func accountInCollection(ac Account, col AccountCollection) bool {
	for _, fol := range col {
		if HashesEqual(fol.Hash, ac.Hash) {
			return true
		}
	}
	return false
}

func (r *repository) loadAccountsFollowers(ctx context.Context, acc *Account) error {
	if !acc.HasMetadata() || len(acc.Metadata.FollowersIRI) == 0 {
		return nil
	}
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Collection(ctx, pub.IRI(acc.Metadata.FollowersIRI), Values(f))
	}
	return LoadFromCollection(ctx, collFn, &colCursor{filters: &Filters{}}, func(o pub.CollectionInterface) (bool, error) {
		for _, fol := range o.Collection() {
			if !pub.ActorTypes.Contains(fol.GetType()) {
				continue
			}
			p := new(Account)
			if err := p.FromActivityPub(fol); err == nil && p.IsValid() {
				acc.Followers = append(acc.Followers, *p)
			}
		}
		return true, nil
	})
}

func (r *repository) loadAccountsFollowing(ctx context.Context, acc *Account) error {
	if !acc.HasMetadata() || len(acc.Metadata.FollowersIRI) == 0 {
		return nil
	}
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Collection(ctx, pub.IRI(acc.Metadata.FollowingIRI), Values(f))
	}
	return LoadFromCollection(ctx, collFn, &colCursor{filters: &Filters{}}, func(o pub.CollectionInterface) (bool, error) {
		for _, fol := range o.Collection() {
			if !pub.ActorTypes.Contains(fol.GetType()) {
				continue
			}
			p := new(Account)
			if err := p.FromActivityPub(fol); err == nil && p.IsValid() {
				acc.Following = append(acc.Following, *p)
			}
		}
		return true, nil
	})
}

func (r *repository) loadAccountsOutbox(ctx context.Context, acc *Account) error {
	if !acc.HasMetadata() || len(acc.Metadata.OutboxIRI) == 0 {
		return nil
	}
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Collection(ctx, pub.IRI(acc.Metadata.OutboxIRI), Values(f))
	}
	return LoadFromCollection(ctx, collFn, &colCursor{filters: &Filters{}}, func(o pub.CollectionInterface) (bool, error) {
		ocTypes := pub.ActivityVocabularyTypes{pub.OrderedCollectionType, pub.OrderedCollectionPageType}
		cTypes := pub.ActivityVocabularyTypes{pub.CollectionType, pub.CollectionPageType}

		if ocTypes.Contains(o.GetType()) {
			pub.OnOrderedCollection(o, func(oc *pub.OrderedCollection) error {
				acc.Metadata.outboxUpdated = oc.Updated
				return nil
			})
		}
		if cTypes.Contains(o.GetType()) {
			pub.OnCollection(o, func(c *pub.Collection) error {
				acc.Metadata.outboxUpdated = c.Updated
				return nil
			})
		}
		for _, it := range o.Collection() {
			acc.Metadata.outbox = append(acc.Metadata.outbox, it)
			typ := it.GetType()
			if ValidAppreciationTypes.Contains(typ) {
				v := new(Vote)
				if err := v.FromActivityPub(it); err == nil && !acc.Votes.Contains(*v) {
					acc.Votes = append(acc.Votes, *v)
				}
			}
			if ValidModerationActivityTypes.Contains(typ) {
				p := new(Account)
				if err := p.FromActivityPub(it); err != nil && !p.IsValid() {
					continue
				}
				if typ == pub.BlockType {
					acc.Blocked = append(acc.Blocked, *p)
				}
				if typ == pub.IgnoreType {
					acc.Ignored = append(acc.Ignored, *p)
				}
			}
		}
		return true, nil
	})
}

func getRepliesOf(items ...Item) pub.IRIs {
	repliesTo := make(pub.IRIs, 0)
	iriFn := func(it Item) pub.IRI {
		if it.pub != nil {
			return it.pub.GetLink()
		}
		if id, ok := BuildIDFromItem(it); ok {
			return id
		}
		return ""
	}
	for _, it := range items {
		if it.OP.IsValid() {
			it = *it.OP
		}
		iri := iriFn(it)
		if len(iri) > 0 && !repliesTo.Contains(iri) {
			repliesTo = append(repliesTo, iri)
		}
	}
	return repliesTo
}

func (r *repository) loadItemsReplies(ctx context.Context, items ...Item) (ItemCollection, error) {
	if len(items) == 0 {
		return nil, nil
	}
	repliesTo := getRepliesOf(items...)
	if len(repliesTo) == 0 {
		return nil, nil
	}
	allReplies := make(ItemCollection, 0)
	f := &Filters{}
	for _, top := range repliesTo {
		collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
			return r.fedbox.Replies(ctx, top.GetLink(), Values(f))
		}
		err := LoadFromCollection(ctx, collFn, &colCursor{filters: f}, func(c pub.CollectionInterface) (bool, error) {
			for _, it := range c.Collection() {
				if !it.IsObject() {
					continue
				}
				i := new(Item)
				if err := i.FromActivityPub(it); err == nil && !allReplies.Contains(*i) {
					allReplies = append(allReplies, *i)
				}
			}
			return true, nil
		})
		if err != nil {
			r.errFn()(err.Error())
		}
	}
	// TODO(marius): probably we can thread the replies right here
	return allReplies, nil
}

func (r *repository) loadAccountVotes(ctx context.Context, acc *Account, items ItemCollection) error {
	if acc == nil || acc.pub == nil {
		return nil
	}
	f := &Filters{
		Object: &Filters{},
		Type:   AppreciationActivitiesFilter,
	}
	for _, it := range items {
		f.Object.IRI = append(f.Object.IRI, LikeString(it.Hash.String()))
	}
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Outbox(ctx, acc.pub, Values(f))
	}
	return LoadFromCollection(ctx, collFn, &colCursor{filters: f}, func(col pub.CollectionInterface) (bool, error) {
		for _, it := range col.Collection() {
			if !it.IsObject() || !ValidAppreciationTypes.Contains(it.GetType()) {
				continue
			}
			v := new(Vote)
			if err := v.FromActivityPub(it); err == nil && !acc.Votes.Contains(*v) {
				acc.Votes = append(acc.Votes, *v)
			}
		}
		return false, nil
	})
}

func (r *repository) loadItemsVotes(ctx context.Context, items ...Item) (ItemCollection, error) {
	if len(items) == 0 {
		return items, nil
	}
	voteActivities := pub.ActivityVocabularyTypes{pub.LikeType, pub.DislikeType, pub.UndoType}
	f := &Filters{
		Object: &Filters{},
		Type:   ActivityTypesFilter(voteActivities...),
	}
	for _, it := range items {
		f.Object.IRI = append(f.Object.IRI, LikeString(it.Hash.String()))
	}
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Inbox(ctx, r.fedbox.Service(), Values(f))
	}
	err := LoadFromCollection(ctx, collFn, &colCursor{filters: f}, func(c pub.CollectionInterface) (bool, error) {
		for _, vAct := range c.Collection() {
			if !vAct.IsObject() || !voteActivities.Contains(vAct.GetType()) {
				continue
			}
			v := new(Vote)
			if err := v.FromActivityPub(vAct); err == nil {
				for k, ob := range items {
					if bytes.Equal(v.Item.Hash, ob.Hash) {
						items[k].Score += v.Weight
					}
				}
			}
		}
		return true, nil
	})
	return items, err
}

func EqualsString(s string) CompStr {
	return CompStr{Operator: "=", Str: s}
}

func ActivityTypesFilter(t ...pub.ActivityVocabularyType) CompStrs {
	r := make(CompStrs, len(t))
	for i, typ := range t {
		r[i] = EqualsString(string(typ))
	}
	return r
}

func (r *repository) loadAccountsAuthors(ctx context.Context, accounts ...Account) ([]Account, error) {
	if len(accounts) == 0 {
		return accounts, nil
	}
	fActors := Filters{
		Type: ActivityTypesFilter(ValidActorTypes...),
	}
	for _, ac := range accounts {
		if !ac.CreatedBy.IsValid() {
			continue
		}
		hash := LikeString(ac.CreatedBy.Hash.String())
		if len(hash.Str) > 0 && !fActors.IRI.Contains(hash) {
			fActors.IRI = append(fActors.IRI, hash)
		}
	}

	if len(fActors.IRI) == 0 {
		return accounts, errors.Errorf("unable to load accounts authors")
	}
	authors, err := r.accounts(ctx, &fActors)
	if err != nil {
		return accounts, errors.Annotatef(err, "unable to load accounts authors")
	}
	for k, ac := range accounts {
		found := false
		for i, auth := range authors {
			if !auth.IsValid() {
				continue
			}
			if accountsEqual(*ac.CreatedBy, *auth) {
				accounts[k].CreatedBy = authors[i]
				found = true
			}
		}
		if !found {
			accounts[k].CreatedBy = &SystemAccount
		}
	}
	return accounts, nil
}

func (r *repository) loadFollowsAuthors(ctx context.Context, items ...FollowRequest) ([]FollowRequest, error) {
	if len(items) == 0 {
		return items, nil
	}
	fActors := Filters{
		Type: ActivityTypesFilter(ValidActorTypes...),
	}
	for _, it := range items {
		if !it.SubmittedBy.IsValid() {
			continue
		}
		hash := LikeString(it.SubmittedBy.Hash.String())
		if len(hash.Str) > 0 && !fActors.IRI.Contains(hash) {
			fActors.IRI = append(fActors.IRI, hash)
		}
	}

	if len(fActors.IRI) == 0 {
		return items, errors.Errorf("unable to load items authors")
	}
	authors, err := r.accounts(ctx, &fActors)
	if err != nil {
		return items, errors.Annotatef(err, "unable to load items authors")
	}
	for k, it := range items {
		for i, auth := range authors {
			if !auth.IsValid() {
				continue
			}
			if accountsEqual(*it.SubmittedBy, *auth) {
				items[k].SubmittedBy = authors[i]
			}
		}
	}
	return items, nil
}

func (r *repository) loadModerationFollowups(ctx context.Context, items []Renderable) ([]ModerationOp, error) {
	inReplyTo := make(pub.IRIs, 0)
	for _, it := range items {
		iri := it.AP().GetLink()
		if !inReplyTo.Contains(iri) {
			inReplyTo = append(inReplyTo, iri)
		}
	}

	modActions := new(Filters)
	modActions.Type = ActivityTypesFilter(pub.DeleteType, pub.UpdateType)
	modActions.InReplTo = IRIsFilter(inReplyTo...)
	modActions.Actor = &Filters{
		IRI: notNilIRIs,
	}
	act, err := r.fedbox.Outbox(ctx, r.fedbox.Service(), Values(modActions))
	if err != nil {
		return nil, err
	}
	modFollowups := make(ModerationOps, 0)
	err = pub.OnCollectionIntf(act, func(c pub.CollectionInterface) error {
		for _, it := range c.Collection() {
			m := new(ModerationOp)
			err := m.FromActivityPub(it)
			if err != nil {
				continue
			}
			if !modFollowups.Contains(*m) {
				modFollowups = append(modFollowups, *m)
			}
		}
		return nil
	})
	return modFollowups, err
}

func (r *repository) loadModerationDetails(ctx context.Context, items ...ModerationOp) ([]ModerationOp, error) {
	if len(items) == 0 {
		return items, nil
	}
	fActors := new(Filters)
	fObjects := new(Filters)
	fActors.IRI = make(CompStrs,0)
	fObjects.IRI = make(CompStrs,0)
	for _, it := range items {
		if !it.SubmittedBy.IsValid() {
			continue
		}
		hash := LikeString(it.SubmittedBy.Hash.String())
		if len(hash.Str) > 0 && !fActors.IRI.Contains(hash) {
			fActors.IRI = append(fActors.IRI, hash)
		}

		ap := it.Object.AP()
		itIRI := ap.GetLink().String()
		if strings.Contains(itIRI, string(actors)) {
			hash = LikeString(path.Base(ap.GetLink().String()))
			if !fActors.IRI.Contains(hash) {
				fActors.IRI = append(fActors.IRI, hash)
			}
		} else if strings.Contains(itIRI, string(objects)) {
			iri := EqualsString(it.Object.AP().GetLink().String())
			if !fObjects.IRI.Contains(iri) {
				fObjects.IRI = append(fObjects.IRI, iri)
			}
		}
	}

	if len(fActors.IRI) == 0 {
		return items, errors.Errorf("unable to load items authors")
	}
	authors, err := r.accounts(ctx, fActors)
	if err != nil {
		return items, errors.Annotatef(err, "unable to load items authors")
	}
	objects, err := r.objects(ctx, fObjects)
	if err != nil {
		return items, errors.Annotatef(err, "unable to load items objects")
	}
	for k, it := range items {
		for i, auth := range authors {
			if !auth.IsValid() {
				continue
			}
			if accountsEqual(*it.SubmittedBy, *auth) {
				items[k].SubmittedBy = authors[i]
			}
			if it.Object.AP().GetLink().Equals(auth.pub.GetLink(), false) {
				items[k].Object = authors[i]
			}
		}
	}
	for k, it := range items {
		for i, obj := range objects {
			if it.Object.AP().GetLink().Equals(obj.pub.GetLink(), false) {
				items[k].Object = &(objects[i])
			}
		}
	}
	return items, nil
}
func accountsEqual(a1, a2 Account) bool {
	return bytes.Equal(a1.Hash, a2.Hash) || (len(a1.Handle)+len(a2.Handle) > 0 && a1.Handle == a2.Handle)
}

func (r *repository) loadItemsAuthors(ctx context.Context, items ...Item) (ItemCollection, error) {
	if len(items) == 0 {
		return items, nil
	}

	fActors := Filters{
		Type: ActivityTypesFilter(ValidActorTypes...),
	}
	for _, it := range items {
		if it.SubmittedBy.IsValid() {
			// Adding an item's author to the list of accounts we want to load from the ActivityPub API
			hash := LikeString(it.SubmittedBy.Hash.String())
			if len(hash.Str) > 0 && !fActors.IRI.Contains(hash) {
				fActors.IRI = append(fActors.IRI, hash)
			}
		}
		if it.HasMetadata() {
			// Adding an item's recipients list (To and CC) to the list of accounts we want to load from the ActivityPub API
			if len(it.Metadata.To) > 0 {
				for _, to := range it.Metadata.To {
					hash := LikeString(to.Hash.String())
					if len(hash.Str) == 0 || fActors.IRI.Contains(hash) {
						continue
					}
					fActors.IRI = append(fActors.IRI, hash)
				}
			}
			if len(it.Metadata.CC) > 0 {
				for _, cc := range it.Metadata.CC {
					hash := LikeString(cc.Hash.String())
					if len(hash.Str) == 0 || fActors.IRI.Contains(hash) {
						continue
					}
					fActors.IRI = append(fActors.IRI, hash)
				}
			}
		}
	}

	if len(fActors.IRI) == 0 {
		return items, nil
	}
	authors, err := r.accounts(ctx, &fActors)
	if err != nil {
		return items, errors.Annotatef(err, "unable to load items authors")
	}
	col := make(ItemCollection, 0)
	for _, it := range items {
		for a := range authors {
			auth := authors[a]
			if !auth.IsValid() {
				continue
			}
			if it.SubmittedBy.IsValid() && HashesEqual(it.SubmittedBy.Hash, auth.Hash) {
				it.SubmittedBy = auth
			}
			if it.UpdatedBy.IsValid() && HashesEqual(it.UpdatedBy.Hash, auth.Hash) {
				it.UpdatedBy = auth
			}
			if !it.HasMetadata() {
				continue
			}
			for i, to := range it.Metadata.To {
				if to.IsValid() && HashesEqual(to.Hash, auth.Hash) {
					it.Metadata.To[i] = auth
				}
			}
			for i, cc := range it.Metadata.CC {
				if cc.IsValid() && HashesEqual(cc.Hash, auth.Hash) {
					it.Metadata.CC[i] = auth
				}
			}
		}
		col = append(col, it)
	}
	return col, nil
}

type Cursor struct {
	after  Hash
	before Hash
	items  RenderableList
	total  uint
}

var emptyCursor = Cursor{}

type colCursor struct {
	filters *Filters
	loaded  int
	items   pub.ItemCollection
}

func getCollectionPrevNext(col pub.CollectionInterface) (prev, next string) {
	qFn := func(i pub.Item) url.Values {
		if i == nil {
			return url.Values{}
		}
		if u, err := i.GetLink().URL(); err == nil {
			return u.Query()
		}
		return url.Values{}
	}
	beforeFn := func(i pub.Item) string {
		return qFn(i).Get("before")
	}
	afterFn := func(i pub.Item) string {
		return qFn(i).Get("after")
	}
	nextFromLastFn := func(i pub.Item) string {
		if u, err := i.GetLink().URL(); err == nil {
			_, next = path.Split(u.Path)
			return next
		}
		return ""
	}
	switch col.GetType() {
	case pub.OrderedCollectionPageType:
		if c, ok := col.(*pub.OrderedCollectionPage); ok {
			prev = beforeFn(c.Prev)
			next = afterFn(c.Next)
		}
	case pub.OrderedCollectionType:
		if c, ok := col.(*pub.OrderedCollection); ok {
			next = afterFn(c.First)
			if next == "" && len(c.OrderedItems) > 0 {
				next = nextFromLastFn(c.OrderedItems[len(c.OrderedItems)-1])
			}
		}
	case pub.CollectionPageType:
		if c, ok := col.(*pub.CollectionPage); ok {
			prev = beforeFn(c.Prev)
			next = afterFn(c.Next)
		}
	case pub.CollectionType:
		if c, ok := col.(*pub.Collection); ok {
			next = afterFn(c.First)
			if next == "" && len(c.Items) > 0 {
				next = nextFromLastFn(c.Items[len(c.Items)-1])
			}
		}
	}
	return prev, next
}

type res struct {
	status accumStatus
	err    error
}

type accumStatus int8

const (
	accumError    accumStatus = -1
	accumContinue accumStatus = iota
	accumSuccess
	accumEndOfCollection
)

// LoadFromCollection iterates over a collection returned by the f function, until accum is satisfied
func LoadFromCollection(ctx context.Context, f CollectionFn, cur *colCursor, accum func(pub.CollectionInterface) (bool, error)) error {
	ttx, tCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	var err error
	processed := 0
	for {
		var status bool
		var col pub.CollectionInterface

		if col, err = f(ttx, cur.filters); err != nil {
			return err
		}

		var prev string
		err = pub.OnCollectionIntf(col, func(c pub.CollectionInterface) error {
			var err error
			status, err = accum(c)
			return err
		})
		if err != nil {
			return err
		}
		prev, cur.filters.Next = getCollectionPrevNext(col)
		if processed == 0 {
			cur.filters.Prev = prev
		}
		processed += len(col.Collection())
		st := accumContinue
		if len(cur.filters.Next) == 0 || uint(processed) == col.Count() {
			st = accumEndOfCollection
		}
		if status {
			st = accumSuccess
		}
		if st != accumContinue {
			tCancel()
			break
		}
	}

	return err
}

func (r *repository) accounts(ctx context.Context, ff ...*Filters) ([]*Account, error) {
	actors := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Actors(ctx, Values(f))
	}
	accounts := make([]*Account, 0)
	// TODO(marius): see how we can use the context returned by errgroup.WithContext()
	g, _ := errgroup.WithContext(ctx)
	for _, f := range ff {
		g.Go(func() error {
			return LoadFromCollection(ctx, actors, &colCursor{filters: f}, func(col pub.CollectionInterface) (bool, error) {
				for _, it := range col.Collection() {
					if !it.IsObject() || !ValidActorTypes.Contains(it.GetType()) {
						continue
					}
					a := new(Account)
					if err := a.FromActivityPub(it); err == nil && a.IsValid() {
						accounts = append(accounts, a)
					}
				}
				// TODO(marius): this needs to be externalized also to a different function that we can pass from outer scope
				//   This function implements the logic for breaking out of the collection iteration cycle and returns a bool
				return len(accounts) == f.MaxItems || len(f.Next) == 0, nil
			})
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func (r *repository) objects(ctx context.Context, ff ...*Filters) (ItemCollection, error) {
	objects := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Objects(ctx, Values(f))
	}
	items := make(ItemCollection, 0)
	// TODO(marius): see how we can use the context returned by errgroup.WithContext()
	g, _ := errgroup.WithContext(ctx)
	for _, f := range ff {
		g.Go(func() error {
			return LoadFromCollection(ctx, objects, &colCursor{filters: f}, func(c pub.CollectionInterface) (bool, error) {
				for _, it := range c.Collection() {
					i := new(Item)
					if err := i.FromActivityPub(it); err == nil && i.IsValid() {
						items = append(items, *i)
					}
				}
				return len(items) == f.MaxItems || len(f.Next) == 0, nil
			})
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *repository) Objects(ctx context.Context, ff ...*Filters) (Cursor, error) {
	items, err := r.objects(ctx, ff...)
	if err != nil {
		return emptyCursor, err
	}
	result := make([]Renderable, 0)
	for _, it := range items {
		if len(it.Hash) > 0 {
			result = append(result, &it)
		}
	}
	var next, prev Hash
	for _, f := range ff {
		next = Hash(f.Next)
		prev = Hash(f.Prev)
	}
	return Cursor{
		after:  next,
		before: prev,
		items:  result,
		total:  uint(len(result)),
	}, nil
}

func validFederated(i Item, f *Filters) bool {
	ob, err := pub.ToObject(i.pub)
	if err != nil {
		return false
	}
	if len(f.Generator) > 0 {
		for _, g := range f.Generator {
			if i.pub == nil || ob.Generator == nil {
				continue
			}
			if g == nilIRI {
				if ob.Generator.GetLink().Equals(pub.IRI(Instance.BaseURL), false) {
					return false
				}
				return true
			}
			if ob.Generator.GetLink().Equals(pub.IRI(g.Str), false) {
				return true
			}
		}
	}
	// @todo(marius): currently this marks as valid nil generator, but we eventually want non nil generators
	return ob != nil && ob.Generator == nil
}

func validRecipients(i Item, f *Filters) bool {
	if len(f.Recipients) > 0 {
		for _, r := range f.Recipients {
			if pub.IRI(r.Str).Equals(pub.PublicNS, false) && i.Private() {
				return false
			}
		}
	}
	return true
}

func validItem(it Item, f *Filters) bool {
	if keep := validRecipients(it, f); !keep {
		return keep
	}
	if keep := validFederated(it, f); !keep {
		return keep
	}
	return true
}

func filterItems(items ItemCollection, f *Filters) ItemCollection {
	result := make(ItemCollection, 0)
	for _, it := range items {
		if !it.HasMetadata() {
			continue
		}
		if validItem(it, f) {
			result = append(result, it)
		}
	}

	return result
}

func IRIsFilter(iris ...pub.IRI) CompStrs {
	r := make(CompStrs, len(iris))
	for i, iri := range iris {
		r[i] = EqualsString(iri.String())
	}
	return r
}

func orderRenderables(r RenderableList) {
	sort.SliceStable(r, func(i, j int) bool {
		return r[i].Date().After(r[j].Date())
	})
}

// ActorCollection loads the service's collection returned by fn.
// First step is to load the Create activities from the inbox
// Iterating over the activities in the resulting collection, we gather the objects and accounts
//  With the resulting Object IRIs we load from the objects collection with our matching filters
//  With the resulting Actor IRIs we load from the accounts collection with matching filters
// From the
func (r *repository) ActorCollection(ctx context.Context, fn CollectionFn, ff ...*Filters) (Cursor, error) {
	items := make(ItemCollection, 0)
	follows := make(FollowRequests, 0)
	accounts := make(AccountCollection, 0)
	moderations := make(ModerationRequests, 0)
	appreciations := make(VoteCollection, 0)
	relations := make(map[pub.IRI]pub.IRI)

	deferredItems := make(CompStrs, 0)
	deferredActors := make(CompStrs, 0)
	deferredActivities := make(CompStrs, 0)

	result := make([]Renderable, 0)
	// TODO(marius): see how we can use the context returned by errgroup.WithContext()
	g, _ := errgroup.WithContext(ctx)
	w := sync.RWMutex{}
	for _, f := range ff {
		g.Go(func() error {
			err := LoadFromCollection(ctx, fn, &colCursor{filters: f}, func(col pub.CollectionInterface) (bool, error) {
				for _, it := range col.Collection() {
					pub.OnActivity(it, func(a *pub.Activity) error {
						typ := it.GetType()
						if typ == pub.CreateType {
							ob := a.Object
							if ob == nil {
								return nil
							}
							if ob.IsObject() {
								if ValidItemTypes.Contains(ob.GetType()) {
									i := new(Item)
									i.FromActivityPub(ob)
									if validItem(*i, f) {
										items = append(items, *i)
									}
								}
								if ValidActorTypes.Contains(ob.GetType()) {
									a := new(Account)
									a.FromActivityPub(ob)
									accounts = append(accounts, *a)
								}
							} else {
								i := new(Item)
								i.FromActivityPub(a)
								uuid := LikeString(path.Base(ob.GetLink().String()))
								if !deferredItems.Contains(uuid) && validItem(*i, f) {
									deferredItems = append(deferredItems, uuid)
								}
							}
							relations[a.GetLink()] = ob.GetLink()
						}
						if it.GetType() == pub.FollowType {
							f := FollowRequest{}
							f.FromActivityPub(a)
							follows = append(follows, f)
							relations[a.GetLink()] = a.GetLink()
						}
						if ValidModerationActivityTypes.Contains(typ) {
							ob := a.Object
							if ob == nil {
								return nil
							}
							if !ob.IsObject() {
								iri := EqualsString(ob.GetLink().String())
								if strings.Contains(iri.String(), string(actors)) && !deferredActors.Contains(iri) {
									deferredActors = append(deferredActors, iri)
								}
								if strings.Contains(iri.String(), string(objects)) && !deferredItems.Contains(iri) {
									deferredItems = append(deferredItems, iri)
								}
								if strings.Contains(iri.String(), string(activities)) && !deferredActivities.Contains(iri) {
									deferredActivities = append(deferredActivities, iri)
								}
							}
							m := ModerationOp{}
							m.FromActivityPub(a)
							moderations = append(moderations, m)
							relations[a.GetLink()] = a.GetLink()
						}
						if ValidAppreciationTypes.Contains(typ) {
							v := Vote{}
							v.FromActivityPub(a)
							appreciations = append(appreciations, v)
							relations[a.GetLink()] = a.GetLink()
						}
						return nil
					})
				}
				// TODO(marius): this needs to be externalized also to a different function that we can pass from outer scope
				//   This function implements the logic for breaking out of the collection iteration cycle and returns a bool
				return len(f.Next) == 0, nil
			})
			if err != nil {
				return err
			}
			if len(deferredItems) > 0 {
				if f.Object == nil {
					f.Object = new(Filters)
				}
				ff := f.Object
				ff.IRI = deferredItems
				objects, _ := r.objects(ctx, ff)
				for _, d := range objects {
					if !items.Contains(d) {
						items = append(items, d)
					}
					for _, m := range moderations {
						modIt := m.Object.AP()
						defIt := d.pub
						if modIt.GetLink().Equals(defIt.GetLink(), false) {
							m.Object = &d
							break
						}
					}
				}
			}
			if len(deferredActors) > 0 {
				if f.Actor == nil {
					f.Actor = new(Filters)
				}
				ff := f.Actor
				ff.IRI = deferredItems
				accounts, _ := r.accounts(ctx, ff)
				for _, d := range accounts {
					if !d.IsValid() {
						continue
					}
					for _, m := range moderations {
						modAc := m.SubmittedBy.AP()
						defAc := d.pub
						if modAc.GetLink().Equals(defAc.GetLink(), false) {
							m.SubmittedBy = d
							break
						}
					}
				}
			}
			items, err = r.loadItemsAuthors(ctx, items...)
			if err != nil {
				return err
			}
			items, err = r.loadItemsVotes(ctx, items...)
			if err != nil {
				return err
			}
			//items, err = r.loadItemsReplies(items...)
			//if err != nil {
			//	return emptyCursor, err
			//}
			follows, err = r.loadFollowsAuthors(ctx, follows...)
			if err != nil {
				return err
			}
			accounts, err = r.loadAccountsAuthors(ctx, accounts...)
			if err != nil {
				return err
			}
			moderations, err = r.loadModerationDetails(ctx, moderations...)
			if err != nil {
				return err
			}
			w.Lock()
			defer w.Unlock()
			for _, rel := range relations {
				for i := range items {
					it := items[i]
					if it.IsValid() && it.pub.GetLink() == rel {
						result = append(result, &it)
						break
					}
				}
				for i := range follows {
					f := follows[i]
					if f.pub != nil && f.pub.GetLink() == rel {
						result = append(result, &f)
					}
				}
				for i := range accounts {
					a := accounts[i]
					if a.pub != nil && a.pub.GetLink() == rel {
						result = append(result, &a)
					}
				}
				for i := range moderations {
					a := moderations[i]
					if a.pub != nil && a.pub.GetLink() == rel {
						result = append(result, &a)
					}
				}
				for i := range appreciations {
					a := appreciations[i]
					if a.pub != nil && a.pub.GetLink() == rel {
						result = append(result, &a)
					}
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return emptyCursor, err
	}
	orderRenderables(result)
	var next, prev Hash
	for _, f := range ff {
		if len(f.Next) > 0 {
			next = Hash(f.Next)
		}
		if len(f.Prev) > 0 {
			prev = Hash(f.Prev)
		}
	}
	return Cursor{
		after:  next,
		before: prev,
		items:  result,
		total:  uint(len(result)),
	}, nil
}

func (r *repository) SaveVote(ctx context.Context, v Vote) (Vote, error) {
	if !v.SubmittedBy.IsValid() || !v.SubmittedBy.HasMetadata() {
		return Vote{}, errors.Newf("Invalid vote submitter")
	}
	if !v.Item.IsValid() || !v.Item.HasMetadata() {
		return Vote{}, errors.Newf("Invalid vote item")
	}
	author := loadAPPerson(*v.SubmittedBy)
	if !accountValidForC2S(v.SubmittedBy) {
		return v, errors.Unauthorizedf("invalid account %s", v.SubmittedBy.Handle)
	}

	url := fmt.Sprintf("%s/%s", v.Item.Metadata.ID, "likes")
	itemVotes, err := r.loadVotesCollection(ctx, pub.IRI(url), pub.IRI(v.SubmittedBy.Metadata.ID))
	// first step is to verify if vote already exists:
	if err != nil {
		r.errFn(log.Ctx{
			"url": url,
			"err": err,
		})(err.Error())
	}
	var exists Vote
	for _, vot := range itemVotes {
		if !vot.SubmittedBy.IsValid() || !v.SubmittedBy.IsValid() {
			continue
		}
		if bytes.Equal(vot.SubmittedBy.Hash, v.SubmittedBy.Hash) {
			exists = vot
			break
		}
	}

	o := new(pub.Object)
	loadAPItem(o, *v.Item)
	act := &pub.Activity{
		Type:  pub.UndoType,
		To:    pub.ItemCollection{pub.PublicNS},
		BCC:   pub.ItemCollection{pub.IRI(BaseURL)},
		Actor: author.GetLink(),
	}

	if exists.HasMetadata() {
		act.Object = pub.IRI(exists.Metadata.IRI)
		if _, _, err := r.fedbox.ToOutbox(ctx, act); err != nil {
			r.errFn()(err.Error())
		}
	}

	if v.Weight > 0 && exists.Weight <= 0 {
		act.Type = pub.LikeType
		act.Object = o.GetLink()
	}
	if v.Weight < 0 && exists.Weight >= 0 {
		act.Type = pub.DislikeType
		act.Object = o.GetLink()
	}

	_, _, err = r.fedbox.ToOutbox(ctx, act)
	if err != nil {
		r.errFn()(err.Error())
		return v, err
	}
	err = v.FromActivityPub(act)
	return v, err
}

func (r *repository) loadVotesCollection(ctx context.Context, iri pub.IRI, actors ...pub.IRI) ([]Vote, error) {
	cntActors := len(actors)
	f := &Filters{}
	if cntActors > 0 {
		f.AttrTo = make(CompStrs, cntActors)
		for i, a := range actors {
			f.AttrTo[i] = LikeString(a.String())
		}
	}
	likes, err := r.fedbox.Collection(ctx, iri, Values(f))
	// first step is to verify if vote already exists:
	if err != nil {
		return nil, err
	}
	votes := make([]Vote, 0)
	err = pub.OnOrderedCollection(likes, func(col *pub.OrderedCollection) error {
		for _, like := range col.OrderedItems {
			vote := Vote{}
			vote.FromActivityPub(like)
			votes = append(votes, vote)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return votes, nil
}

type _errors struct {
	Ctxt   string        `jsonld:"@context"`
	Errors []errors.Http `jsonld:"errors"`
}

func (r *repository) handlerErrorResponse(body []byte) error {
	errs := _errors{}
	if err := j.Unmarshal(body, &errs); err != nil {
		r.errFn()("Unable to unmarshal error response: %s", err.Error())
		return nil
	}
	if len(errs.Errors) == 0 {
		return nil
	}
	err := errs.Errors[0]
	return errors.WrapWithStatus(err.Code, nil, err.Message)
}

func (r *repository) handleItemSaveSuccessResponse(ctx context.Context, it Item, body []byte) (Item, error) {
	ap, err := pub.UnmarshalJSON(body)
	if err != nil {
		r.errFn()(err.Error())
		return it, err
	}
	err = it.FromActivityPub(ap)
	if err != nil {
		r.errFn()(err.Error())
		return it, err
	}
	items, err := r.loadItemsAuthors(ctx, it)
	return items[0], err
}

func accountValidForC2S(a *Account) bool {
	return a.IsValid() /*&& a.IsLogged()*/
}

func (r *repository) getAuthorRequestURL(a *Account) string {
	var reqURL string
	if a.IsValid() && a.IsLogged() {
		author := loadAPPerson(*a)
		if a.IsLocal() {
			reqURL = author.Outbox.GetLink().String()
		} else {
			reqURL = author.Inbox.GetLink().String()
		}
	} else {
		author := anonymousPerson(r.BaseURL)
		reqURL = author.Inbox.GetLink().String()
	}
	return reqURL
}

func (r *repository) SaveItem(ctx context.Context, it Item) (Item, error) {
	if it.SubmittedBy == nil || !it.SubmittedBy.HasMetadata() {
		return Item{}, errors.Newf("invalid account")
	}
	art := new(pub.Object)
	loadAPItem(art, it)
	var author *pub.Actor
	if it.SubmittedBy.IsLogged() {
		author = loadAPPerson(*it.SubmittedBy)
	} else {
		author = anonymousPerson(r.BaseURL)
	}
	if !accountValidForC2S(it.SubmittedBy) {
		return it, errors.Unauthorizedf("invalid account %s", it.SubmittedBy.Handle)
	}

	to := make(pub.ItemCollection, 0)
	cc := make(pub.ItemCollection, 0)
	bcc := make(pub.ItemCollection, 0)

	var err error
	id := art.GetLink()

	if it.HasMetadata() {
		m := it.Metadata
		if len(m.To) > 0 {
			for _, rec := range m.To {
				to = append(to, pub.IRI(rec.Metadata.ID))
			}
		}
		if len(m.CC) > 0 {
			for _, rec := range m.CC {
				cc = append(cc, pub.IRI(rec.Metadata.ID))
			}
		}
		if len(it.Metadata.Mentions) > 0 {
			names := make(CompStrs, 0)
			for _, m := range it.Metadata.Mentions {
				names = append(names, EqualsString(m.Name))
			}
			ff := &Filters{Name: names}
			actors, _, err := r.LoadAccounts(ctx, ff)
			if err != nil {
				r.errFn(log.Ctx{"err": err})("unable to load accounts from mentions")
			}
			for _, actor := range actors {
				if actor.HasMetadata() && len(actor.Metadata.ID) > 0 {
					cc = append(cc, pub.IRI(actor.Metadata.ID))
				}
			}
		}
	}

	if !it.Private() {
		to = append(to, pub.PublicNS)
		if it.Parent == nil && it.SubmittedBy.HasMetadata() && len(it.SubmittedBy.Metadata.FollowersIRI) > 0 {
			cc = append(cc, pub.IRI(it.SubmittedBy.Metadata.FollowersIRI))
		}
		bcc = append(bcc, pub.IRI(BaseURL))
	}

	act := &pub.Activity{
		To:     to,
		CC:     cc,
		BCC:    bcc,
		Actor:  author.GetLink(),
		Object: art,
	}
	loadAuthors := true
	if it.Deleted() {
		if len(id) == 0 {
			r.errFn(log.Ctx{
				"item": it.Hash,
			})(err.Error())
			return it, errors.NotFoundf("item hash is empty, can not delete")
		}
		act.Object = id
		act.Type = pub.DeleteType
		loadAuthors = false
	} else {
		if len(id) == 0 {
			act.Type = pub.CreateType
		} else {
			act.Type = pub.UpdateType
		}
	}
	var ob pub.Item
	_, ob, err = r.fedbox.ToOutbox(ctx, act)
	if err != nil {
		r.errFn()(err.Error())
		return it, err
	}
	err = it.FromActivityPub(ob)
	if err != nil {
		r.errFn()(err.Error())
		return it, err
	}
	if loadAuthors {
		items, err := r.loadItemsAuthors(ctx, it)
		return items[0], err
	}
	return it, err
}

func (r *repository) LoadAccounts(ctx context.Context, ff ...*Filters) (AccountCollection, uint, error) {
	accounts := make(AccountCollection, 0)
	var count uint = 0
	// TODO(marius): see how we can use the context returned by errgroup.WithContext()
	g, _ := errgroup.WithContext(ctx)
	for _, f := range ff {
		g.Go(func() error {
			it, err := r.fedbox.Actors(ctx, Values(f))
			if err != nil {
				r.errFn()(err.Error())
				return err
			}
			return pub.OnOrderedCollection(it, func(col *pub.OrderedCollection) error {
				count = col.TotalItems
				for _, it := range col.OrderedItems {
					acc := Account{Metadata: &AccountMetadata{}}
					if err := acc.FromActivityPub(it); err != nil {
						r.errFn(log.Ctx{
							"type": fmt.Sprintf("%T", it),
						})(err.Error())
						continue
					}
					accounts = append(accounts, acc)
				}
				accounts, err = r.loadAccountsVotes(ctx, accounts...)
				return err
			})
		})
	}
	if err := g.Wait(); err != nil {
		return accounts, count, err
	}
	return accounts, count, nil
}

func (r *repository) LoadAccountDetails(ctx context.Context, acc *Account) error {
	r.WithAccount(acc)
	ltx := log.Ctx{
		"handle": acc.Handle,
		"hash":   acc.Hash,
	}
	var err error
	if len(acc.Followers) == 0 {
		// TODO(marius): this needs to be moved to where we're handling all Inbox activities, not on page load
		if err = r.loadAccountsFollowers(ctx, acc); err != nil {
			r.infoFn(ltx, log.Ctx{"err": err.Error()})("unable to load followers")
		}
	}
	if len(acc.Following) == 0 {
		if err = r.loadAccountsFollowing(ctx, acc); err != nil {
			r.infoFn(ltx, log.Ctx{"err": err.Error()})("unable to load following")
		}
	}
	if err = r.loadAccountsOutbox(ctx, acc); err != nil {
		r.infoFn(ltx, log.Ctx{"err": err.Error()})("unable to load outbox")
	}
	return nil
}

func (r *repository) LoadAccount(ctx context.Context, iri pub.IRI) (*Account, error) {
	acc := new(Account)
	act, err := r.fedbox.Actor(ctx, iri)
	if err != nil {
		r.errFn()(err.Error())
		return acc, err
	}
	err = acc.FromActivityPub(act)
	if err != nil {
		return acc, err
	}
	err = r.LoadAccountDetails(ctx, acc)
	return acc, err
}

func Values(f interface{}) func() url.Values {
	return func() url.Values {
		v, e := qstring.Marshal(f)
		if e != nil {
			return url.Values{}
		}
		return v
	}
}

func (r *repository) LoadFollowRequests(ctx context.Context, ed *Account, f *Filters) (FollowRequests, uint, error) {
	if len(f.Type) == 0 {
		f.Type = ActivityTypesFilter(pub.FollowType)
	}
	var followReq pub.CollectionInterface
	var err error
	if ed == nil {
		followReq, err = r.fedbox.Activities(ctx, Values(f))
	} else {
		followReq, err = r.fedbox.Inbox(ctx, loadAPPerson(*ed), Values(f))
	}
	requests := make([]FollowRequest, 0)
	if err == nil && len(followReq.Collection()) > 0 {
		for _, fr := range followReq.Collection() {
			f := new(FollowRequest)
			if err := f.FromActivityPub(fr); err == nil {
				if !accountInCollection(*f.SubmittedBy, ed.Followers) {
					requests = append(requests, *f)
				}
			}
		}
		requests, err = r.loadFollowsAuthors(ctx, requests...)
	}
	return requests, uint(len(requests)), nil
}

func (r *repository) SendFollowResponse(ctx context.Context, f FollowRequest, accept bool, reason *Item) error {
	er := f.SubmittedBy
	if !er.IsValid() {
		return errors.Newf("invalid account to follow %s", er.Handle)
	}
	ed := f.Object
	if !accountValidForC2S(ed) {
		return errors.Unauthorizedf("invalid account for request %s", ed.Handle)
	}

	to := make(pub.ItemCollection, 0)
	bcc := make(pub.ItemCollection, 0)

	to = append(to, pub.IRI(er.Metadata.ID))
	bcc = append(bcc, pub.IRI(BaseURL))

	response := new(pub.Activity)
	if reason != nil {
		loadAPItem(response, *reason)
	}
	response.To = to
	response.Type = pub.RejectType
	response.BCC = bcc
	response.Object = pub.IRI(f.Metadata.ID)
	response.Actor = pub.IRI(ed.Metadata.ID)
	if accept {
		to = append(to, pub.PublicNS)
		response.Type = pub.AcceptType
	}

	_, _, err := r.fedbox.ToOutbox(ctx, response)
	if err != nil {
		r.errFn(log.Ctx{
			"err":      err,
			"follower": er.Handle,
			"followed": ed.Handle,
		})("unable to respond to follow")
		return err
	}
	return nil
}

func (r *repository) FollowAccount(ctx context.Context, er, ed Account, reason *Item) error {
	follower := loadAPPerson(er)
	followed := loadAPPerson(ed)
	if !accountValidForC2S(&er) {
		return errors.Unauthorizedf("invalid account %s", er.Handle)
	}

	to := make(pub.ItemCollection, 0)
	bcc := make(pub.ItemCollection, 0)

	//to = append(to, follower.GetLink())
	to = append(to, pub.PublicNS)
	bcc = append(bcc, pub.IRI(BaseURL))

	follow := new(pub.Follow)
	if reason != nil {
		loadAPItem(follow, *reason)
	}
	follow.Type = pub.FollowType
	follow.To = to
	follow.BCC = bcc
	follow.Object = followed.GetLink()
	follow.Actor = follower.GetLink()
	_, _, err := r.fedbox.ToOutbox(ctx, follow)
	if err != nil {
		r.errFn(log.Ctx{
			"err":      err,
			"follower": er.Handle,
			"followed": ed.Handle,
		})("Unable to follow")
		return err
	}
	return nil
}

func (r *repository) SaveAccount(ctx context.Context, a Account) (Account, error) {
	p := loadAPPerson(a)
	id := p.GetLink()

	now := time.Now().UTC()

	if p.Published.IsZero() {
		p.Published = now
	}
	p.Updated = now

	act := &pub.Activity{
		To:      pub.ItemCollection{pub.PublicNS},
		BCC:     pub.ItemCollection{pub.IRI(BaseURL)},
		Updated: now,
	}

	var author *pub.Actor
	if a.CreatedBy != nil {
		author = loadAPPerson(*a.CreatedBy)
	} else {
		author = r.fedbox.Service()
	}
	act.AttributedTo = author.GetLink()
	act.Actor = author.GetLink()
	var err error
	if a.Deleted() {
		if len(id) == 0 {
			err := errors.NotFoundf("item hash is empty, can not delete")
			r.infoFn(log.Ctx{
				"actor":  a.GetLink(),
				"author": author.GetLink(),
				"err":    err,
			})("save failed")
			return a, err
		}
		act.Type = pub.DeleteType
		act.Object = id
	} else {
		act.Object = p
		p.To = pub.ItemCollection{pub.PublicNS}
		p.BCC = pub.ItemCollection{pub.IRI(BaseURL)}
		if len(id) == 0 {
			act.Type = pub.CreateType
		} else {
			act.Type = pub.UpdateType
		}
	}

	var ap pub.Item
	if _, ap, err = r.fedbox.ToOutbox(ctx, act); err != nil {
		ltx := log.Ctx{
			"author": author.GetLink(),
			"err":    err,
		}
		if ap != nil {
			ltx["activity"] = ap.GetLink()
		}
		r.errFn(ltx)("save failed")
		return a, err
	}
	err = a.FromActivityPub(ap)
	if err != nil {
		r.errFn(log.Ctx{
			"actor": a.Handle,
			"err":   err,
		})("loading of actor from JSON failed")
	}
	return a, err
}

// LoadInfo this method is here to keep compatibility with the repository interfaces
// but in the long term we might want to store some of this information in the DB
func (r *repository) LoadInfo() (WebInfo, error) {
	return Instance.NodeInfo(), nil
}

func (r *repository) LoadAccountWithDetails(ctx context.Context, actor *Account, f ...*Filters) (*Cursor, error) {
	c, err := r.LoadActorOutbox(ctx, actor.pub, f...)
	if err != nil {
		return c, err
	}
	remaining := make(RenderableList, 0)
	for _, it := range c.items {
		switch it.Type() {
		case AppreciationType:
			v, ok := it.(*Vote)
			if !ok {
				continue
			}
			if actor.Votes.Contains(*v) {
				continue
			}
			actor.Votes = append(actor.Votes, *v)
		default:
			remaining = append(remaining, it)
		}
	}
	c.items = remaining
	c.total = uint(len(remaining))
	return c, nil
}

func (r *repository) LoadActorOutbox(ctx context.Context, actor pub.Item, f ...*Filters) (*Cursor, error) {
	if actor == nil {
		return nil, errors.Errorf("Invalid actor")
	}
	outbox := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Outbox(ctx, actor, Values(f))
	}
	cursor, err := r.ActorCollection(ctx, outbox, f...)
	if err != nil {
		return nil, err
	}
	return &cursor, nil
}

func (r *repository) LoadActivities(ctx context.Context, ff ...*Filters) (*Cursor, error) {
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Activities(ctx, Values(f))
	}
	cursor, err := r.ActorCollection(ctx, collFn, ff...)
	if err != nil {
		return nil, err
	}
	return &cursor, nil
}

func (r *repository) LoadActorInbox(ctx context.Context, actor pub.Item, f ...*Filters) (*Cursor, error) {
	if actor == nil {
		return nil, errors.Errorf("Invalid actor")
	}
	collFn := func(ctx context.Context, f *Filters) (pub.CollectionInterface, error) {
		return r.fedbox.Inbox(ctx, actor, Values(f))
	}
	cursor, err := r.ActorCollection(ctx, collFn, f...)
	if err != nil {
		return nil, err
	}
	return &cursor, nil
}

func (r repository) moderationActivity(ctx context.Context, er *pub.Actor, ed pub.Item, reason *Item) (*pub.Activity, error) {
	bcc := make(pub.ItemCollection, 0)
	bcc = append(bcc, pub.IRI(BaseURL))

	// We need to add the ed/er accounts' creators to the CC list
	cc := make(pub.ItemCollection, 0)
	if er.AttributedTo != nil && !er.AttributedTo.GetLink().Equals(pub.PublicNS, true) {
		cc = append(cc, er.AttributedTo.GetLink())
	}
	pub.OnObject(ed, func(o *pub.Object) error {
		if o.AttributedTo != nil {
			auth, err := r.fedbox.Actor(ctx, o.AttributedTo.GetLink())
			if err == nil && auth.AttributedTo != nil &&
				!(auth.AttributedTo.GetLink().Equals(auth.GetLink(), false) || auth.AttributedTo.GetLink().Equals(pub.PublicNS, true)) {
				cc = append(cc, auth.AttributedTo.GetLink())
			}
		}
		return nil
	})

	act := new(pub.Activity)
	if reason != nil {
		loadAPItem(act, *reason)
	}
	act.BCC = bcc
	act.CC = cc
	act.Object = ed.GetLink()
	act.Actor = er.GetLink()
	return act, nil
}

func (r repository) moderationActivityOnItem(ctx context.Context, er Account, ed Item, reason *Item) (*pub.Activity, error) {
	reporter := loadAPPerson(er)
	reported := new(pub.Object)
	loadAPItem(reported, ed)
	if !accountValidForC2S(&er) {
		return nil, errors.Unauthorizedf("invalid account %s", er.Handle)
	}
	return r.moderationActivity(ctx, reporter, reported, reason)
}

func (r repository) moderationActivityOnAccount(ctx context.Context, er, ed Account, reason *Item) (*pub.Activity, error) {
	reporter := loadAPPerson(er)
	reported := loadAPPerson(ed)
	if !accountValidForC2S(&er) {
		return nil, errors.Unauthorizedf("invalid account %s", er.Handle)
	}

	return r.moderationActivity(ctx, reporter, reported, reason)
}

func (r *repository) BlockAccount(ctx context.Context, er, ed Account, reason *Item) error {
	block, err := r.moderationActivityOnAccount(ctx, er, ed, reason)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	block.Type = pub.BlockType
	_, _, err = r.fedbox.ToOutbox(ctx, block)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	return nil
}

func (r *repository) BlockItem(ctx context.Context, er Account, ed Item, reason *Item) error {
	block, err := r.moderationActivityOnItem(ctx, er, ed, reason)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	block.Type = pub.BlockType
	_, _, err = r.fedbox.ToOutbox(ctx, block)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	return nil
}

func (r *repository) ReportItem(ctx context.Context, er Account, it Item, reason *Item) error {
	flag, err := r.moderationActivityOnItem(ctx, er, it, reason)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	flag.Type = pub.FlagType
	_, _, err = r.fedbox.ToOutbox(ctx, flag)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	return nil
}

func (r *repository) ReportAccount(ctx context.Context, er, ed Account, reason *Item) error {
	report, err := r.moderationActivityOnAccount(ctx, er, ed, reason)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	report.Type = pub.FlagType
	_, _, err = r.fedbox.ToOutbox(ctx, report)
	if err != nil {
		r.errFn()(err.Error())
		return err
	}
	return nil
}
