// Copyright 2026 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// The connection probe and the server-side classifier predicate were removed
// in review round 11: the classifier misclassifies upgraded RENAME rows and
// USE-qualified statements, never inspects empty-kind index rows, and every
// outcome it can produce is dominated by the unified __mo_tmp_<32hex>_
// ambiguity gate in getTables (explicit -tbl export, otherwise abort).
