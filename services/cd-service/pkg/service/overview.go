/*This file is part of kuberpult.

Kuberpult is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

Kuberpult is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with kuberpult.  If not, see <http://www.gnu.org/licenses/>.

Copyright 2021 freiheit.com*/
package service

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"

	"github.com/freiheit-com/fdc-continuous-delivery/pkg/api"
	"github.com/freiheit-com/fdc-continuous-delivery/services/cd-service/pkg/config"
	"github.com/freiheit-com/fdc-continuous-delivery/services/cd-service/pkg/repository"
	git "github.com/libgit2/git2go/v31"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type OverviewServiceServer struct {
	Repository *repository.Repository
	Shutdown   <-chan struct{}

	mx       sync.Mutex
	listener map[chan struct{}]struct{}

	init     sync.Once
	response atomic.Value
}

func (o *OverviewServiceServer) GetOverview(
	ctx context.Context,
	in *api.GetOverviewRequest) (*api.GetOverviewResponse, error) {
	return o.getOverview(ctx, o.Repository.State())
}

func (o *OverviewServiceServer) getOverview(
	ctx context.Context,
	s *repository.State) (*api.GetOverviewResponse, error) {
	result := api.GetOverviewResponse{
		Environments: map[string]*api.Environment{},
		Applications: map[string]*api.Application{},
	}
	if envs, err := s.GetEnvironmentConfigs(); err != nil {
		return nil, internalError(ctx, err)
	} else {
		for name, config := range envs {
			env := api.Environment{
				Name: name,
				Config: &api.Environment_Config{
					Upstream: transformUpstream(config.Upstream),
				},
				Locks:        map[string]*api.Lock{},
				Applications: map[string]*api.Environment_Application{},
			}
			if locks, err := s.GetEnvironmentLocks(name); err != nil {
				return nil, err
			} else {
				for lockId, lock := range locks {
					env.Locks[lockId] = &api.Lock{
						Message: lock.Message,
					}
				}
			}
			if apps, err := s.GetEnvironmentApplications(name); err != nil {
				return nil, err
			} else {
				for _, appName := range apps {
					app := api.Environment_Application{
						Name:  appName,
						Locks: map[string]*api.Lock{},
					}
					if version, err := s.GetEnvironmentApplicationVersion(name, appName); err != nil && !errors.Is(err, os.ErrNotExist) {
						return nil, err
					} else {
						if version == nil {
							app.Version = 0
						} else {
							app.Version = *version
						}
					}
					if commit, err := s.GetEnvironmentApplicationVersionCommit(name, appName); err != nil {
						return nil, err
					} else {
						app.VersionCommit = transformCommit(commit)
					}
					if queuedVersion, err := s.GetQueuedVersion(name, appName); err != nil && !errors.Is(err, os.ErrNotExist) {
						return nil, err
					} else {
						if queuedVersion == nil {
							app.QueuedVersion = 0
						} else {
							app.QueuedVersion = *queuedVersion
						}
					}
					if appLocks, err := s.GetEnvironmentApplicationLocks(name, appName); err != nil {
						return nil, err
					} else {
						for lockId, lock := range appLocks {
							app.Locks[lockId] = &api.Lock{
								Message: lock.Message,
							}
						}
					}

					env.Applications[appName] = &app
				}
			}
			result.Environments[name] = &env
		}
	}
	if apps, err := s.GetApplications(); err != nil {
		return nil, err
	} else {
		for _, appName := range apps {
			app := api.Application{
				Name:     appName,
				Releases: []*api.Release{},
			}
			if rels, err := s.GetApplicationReleases(appName); err != nil {
				return nil, err
			} else {
				for _, id := range rels {
					if rel, err := s.GetApplicationRelease(appName, id); err != nil {
						return nil, err
					} else {
						release := &api.Release{
							Version:        id,
							SourceAuthor:   rel.SourceAuthor,
							SourceCommitId: rel.SourceCommitId,
							SourceMessage:  rel.SourceMessage,
						}
						if commit, err := s.GetApplicationReleaseCommit(appName,id) ; err != nil {
							return nil, err
						} else {
							release.Commit = transformCommit(commit)
						}
						app.Releases = append(app.Releases, release)
					}
				}
			}
			result.Applications[appName] = &app
		}
	}
	return &result, nil
}

func (o *OverviewServiceServer) StreamOverview(in *api.GetOverviewRequest,
	stream api.OverviewService_StreamOverviewServer) error {
	ch, unsubscribe := o.subscribe()
	defer unsubscribe()
	done := stream.Context().Done()
	for {
		select {
		case <-o.Shutdown:
			return nil
		case <-ch:
			ov := o.response.Load().(*api.GetOverviewResponse)
			if err := stream.Send(ov); err != nil {
				return err
			}
		case <-done:
			return nil
		}
	}
}

type unsubscribe = func()

func (o *OverviewServiceServer) subscribe() (<-chan struct{}, unsubscribe) {
	o.init.Do(func() {
		o.Repository.SetCallback(o.update)
		o.update(o.Repository.State())
	})

	ch := make(chan struct{}, 1)
	ch <- struct{}{}

	o.mx.Lock()
	defer o.mx.Unlock()
	if o.listener == nil {
		o.listener = map[chan struct{}]struct{}{}
	}

	o.listener[ch] = struct{}{}
	return ch, func() {
		o.mx.Lock()
		defer o.mx.Unlock()
		delete(o.listener, ch)
	}
}

func (o *OverviewServiceServer) update(s *repository.State) {
	r, err := o.getOverview(context.Background(), s)
	if err != nil {
		panic(err)
	}
	o.response.Store(r)
	o.mx.Lock()
	defer o.mx.Unlock()
	for ch := range o.listener {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func transformUpstream(upstream *config.EnvironmentConfigUpstream) *api.Environment_Config_Upstream {
	if upstream == nil {
		return nil
	}
	if upstream.Latest {
		return &api.Environment_Config_Upstream{
			Upstream: &api.Environment_Config_Upstream_Latest{
				Latest: upstream.Latest,
			},
		}
	}
	if upstream.Environment != "" {
		return &api.Environment_Config_Upstream{
			Upstream: &api.Environment_Config_Upstream_Environment{
				Environment: upstream.Environment,
			},
		}
	}
	return nil
}

func transformCommit(commit *git.Commit) *api.Commit {
	if( commit == nil ) {
		return nil
	}
	author := commit.Author()
	return &api.Commit{
		AuthorName: author.Name,
		AuthorEmail: author.Email,
		AuthorTime:  timestamppb.New(author.When),
	}
}
