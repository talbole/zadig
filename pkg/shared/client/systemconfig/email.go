/*
Copyright 2021 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package systemconfig

import (
	emailservice "github.com/koderover/zadig/pkg/microservice/systemconfig/core/email/service"
	"github.com/koderover/zadig/pkg/tool/log"
)

type Email struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	UserName string `json:"username"`
	Password string `json:"password"`
}

func (c *Client) GetEmailHost() (*Email, error) {
	resp, err := emailservice.GetEmailHostInternal(log.SugaredLogger())
	if err != nil {
		return nil, err
	}
	res := &Email{
		Name:     resp.Name,
		Port:     resp.Port,
		UserName: resp.Username,
		Password: resp.Password,
	}

	return res, err
}
