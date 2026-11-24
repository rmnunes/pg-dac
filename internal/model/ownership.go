package model

// OwnedObject is an object with an owner. Linked marks a sequence owned by a
// column, which PostgreSQL keeps under its table's owner (an ALTER … OWNER TO
// of the table moves it); indexes and the row types of relations are not
// listed at all. Function is set for routines (ALTER FUNCTION or PROCEDURE).
type OwnedObject struct {
	Ref      ObjRef
	Owner    string
	Linked   bool
	Function *Function
	// set writes a new owner back to the model object.
	set func(string)
}

// SetOwner records a new owner on the model object.
func (o OwnedObject) SetOwner(role string) { o.set(role) }

// OwnedObjects lists the database's objects that have an owner, schemas
// first, in model order.
func (d *Database) OwnedObjects() []OwnedObject {
	var out []OwnedObject
	add := func(ref ObjRef, owner *string, fn *Function) {
		out = append(out, OwnedObject{Ref: ref, Owner: *owner, Function: fn, set: func(r string) { *owner = r }})
	}
	if d == nil {
		return nil
	}
	for _, s := range d.Schemas {
		add(ObjRef{Kind: KindSchema, Name: s.Name}, &s.Owner, nil)
	}
	for _, s := range d.Schemas {
		for _, c := range s.Collations {
			add(c.Ref(), &c.Owner, nil)
		}
		for _, e := range s.Enums {
			add(e.Ref(), &e.Owner, nil)
		}
		for _, x := range s.Domains {
			add(x.Ref(), &x.Owner, nil)
		}
		for _, c := range s.Composites {
			add(c.Ref(), &c.Owner, nil)
		}
		for _, f := range s.Functions {
			add(f.Ref(), &f.Owner, f)
		}
		for _, q := range s.Sequences {
			add(q.Ref(), &q.Owner, nil)
			out[len(out)-1].Linked = q.IsOwned()
		}
		for _, t := range s.Tables {
			add(t.Ref(), &t.Owner, nil)
		}
		for _, v := range s.Views {
			add(v.Ref(), &v.Owner, nil)
		}
		for _, m := range s.MatViews {
			add(m.Ref(), &m.Owner, nil)
		}
	}
	return out
}

// Owners maps every ownable object's key to its owner.
func (d *Database) Owners() map[string]string {
	out := map[string]string{}
	if d == nil {
		return out
	}
	for _, o := range d.OwnedObjects() {
		out[o.Ref.Key()] = o.Owner
	}
	return out
}

// CommonOwner returns the role that owns every ownable object, or "" when
// they have different owners or there are none. The public schema is not
// counted: every database comes with it, owned by pg_database_owner or the
// bootstrap superuser, whoever owns what is in it.
func (d *Database) CommonOwner() string {
	owner := ""
	for _, o := range d.OwnedObjects() {
		if o.Ref.Kind == KindSchema && o.Ref.Name == "public" {
			continue
		}
		if o.Owner == "" {
			continue
		}
		if owner != "" && o.Owner != owner {
			return ""
		}
		owner = o.Owner
	}
	return owner
}
