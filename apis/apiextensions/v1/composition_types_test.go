/*
Copyright 2020 The Crossplane Authors.

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

package v1

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	"github.com/crossplane/crossplane-runtime/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/pkg/test"
)

func TestPatchTypeReplacement(t *testing.T) {
	type args struct {
		comp CompositionSpec
	}

	type want struct {
		resources []ComposedTemplate
		err       error
	}

	cases := map[string]struct {
		reason string
		args
		want
	}{
		"NoCompositionPatchSets": {
			reason: "Patches defined on a composite resource should be applied correctly if no PatchSets are defined on the composition",
			args: args{
				comp: CompositionSpec{
					Resources: []ComposedTemplate{
						{
							Patches: []Patch{
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("metadata.name"),
								},
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("metadata.namespace"),
								},
							},
						},
					},
				},
			},
			want: want{
				resources: []ComposedTemplate{
					{
						Patches: []Patch{
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.name"),
							},
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.namespace"),
							},
						},
					},
				},
			},
		},
		"UndefinedPatchSet": {
			reason: "Should return error and not modify the patches field when referring to an undefined PatchSet",
			args: args{
				comp: CompositionSpec{
					Resources: []ComposedTemplate{{
						Patches: []Patch{
							{
								Type:         PatchTypePatchSet,
								PatchSetName: pointer.StringPtr("patch-set-1"),
							},
						},
					}},
				},
			},
			want: want{
				resources: []ComposedTemplate{{
					Patches: []Patch{
						{
							Type:         PatchTypePatchSet,
							PatchSetName: pointer.StringPtr("patch-set-1"),
						},
					},
				}},
				err: errors.Errorf(errUndefinedPatchSet, "patch-set-1"),
			},
		},
		"DefinedPatchSets": {
			reason: "Should de-reference PatchSets defined on the Composition when referenced in a composed resource",
			args: args{
				comp: CompositionSpec{
					// PatchSets, existing patches and references
					// should output in the correct order.
					PatchSets: []PatchSet{
						{
							Name: "patch-set-1",
							Patches: []Patch{
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("metadata.namespace"),
								},
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("spec.parameters.test"),
								},
							},
						},
						{
							Name: "patch-set-2",
							Patches: []Patch{
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("metadata.annotations.patch-test-1"),
								},
								{
									Type: PatchTypeFromConstantValue,
									ConstantValue: &ConstantValue{
										Type:   ConstantTypeString,
										String: pointer.StringPtr("constant-value"),
									},
								},
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("metadata.annotations.patch-test-2"),
									Transforms: []Transform{{
										Type: TransformTypeMap,
										Map: &MapTransform{
											Pairs: map[string]string{
												"k-1": "v-1",
												"k-2": "v-2",
											},
										},
									}},
								},
							},
						},
					},
					Resources: []ComposedTemplate{
						{
							Patches: []Patch{
								{
									Type:         PatchTypePatchSet,
									PatchSetName: pointer.StringPtr("patch-set-2"),
								},
								{
									Type:          PatchTypeFromCompositeFieldPath,
									FromFieldPath: pointer.StringPtr("metadata.name"),
								},
								{
									Type:         PatchTypePatchSet,
									PatchSetName: pointer.StringPtr("patch-set-1"),
								},
							},
						},
						{
							Patches: []Patch{
								{
									Type:         PatchTypePatchSet,
									PatchSetName: pointer.StringPtr("patch-set-1"),
								},
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				resources: []ComposedTemplate{
					{
						Patches: []Patch{
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.annotations.patch-test-1"),
							},
							{
								Type: PatchTypeFromConstantValue,
								ConstantValue: &ConstantValue{
									Type:   ConstantTypeString,
									String: pointer.StringPtr("constant-value"),
								},
							},
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.annotations.patch-test-2"),
								Transforms: []Transform{{
									Type: TransformTypeMap,
									Map: &MapTransform{
										Pairs: map[string]string{
											"k-1": "v-1",
											"k-2": "v-2",
										},
									},
								}},
							},
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.name"),
							},
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.namespace"),
							},
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("spec.parameters.test"),
							},
						},
					},
					{
						Patches: []Patch{
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("metadata.namespace"),
							},
							{
								Type:          PatchTypeFromCompositeFieldPath,
								FromFieldPath: pointer.StringPtr("spec.parameters.test"),
							},
						},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.args.comp.InlinePatchSets()

			if diff := cmp.Diff(tc.want.resources, tc.args.comp.Resources); diff != "" {
				t.Errorf("\n%s\nInlinePatchSets(b): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nInlinePatchSets(b): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestMapResolve(t *testing.T) {
	type args struct {
		m map[string]string
		i interface{}
	}
	type want struct {
		o   interface{}
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"NonStringInput": {
			args: args{
				i: 5,
			},
			want: want{
				err: errors.Errorf(errFmtMapTypeNotSupported, "int"),
			},
		},
		"KeyNotFound": {
			args: args{
				i: "ola",
			},
			want: want{
				err: errors.Errorf(errFmtMapNotFound, "ola"),
			},
		},
		"Success": {
			args: args{
				m: map[string]string{"ola": "voila"},
				i: "ola",
			},
			want: want{
				o: "voila",
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := (&MapTransform{Pairs: tc.m}).Resolve(tc.i)

			if diff := cmp.Diff(tc.want.o, got); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestMathResolve(t *testing.T) {
	m := int64(2)

	type args struct {
		multiplier *int64
		i          interface{}
	}
	type want struct {
		o   interface{}
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"NoMultiplier": {
			args: args{
				i: 25,
			},
			want: want{
				err: errors.New(errMathNoMultiplier),
			},
		},
		"NonNumberInput": {
			args: args{
				multiplier: &m,
				i:          "ola",
			},
			want: want{
				err: errors.New(errMathInputNonNumber),
			},
		},
		"Success": {
			args: args{
				multiplier: &m,
				i:          3,
			},
			want: want{
				o: 3 * m,
			},
		},
		"SuccessInt64": {
			args: args{
				multiplier: &m,
				i:          int64(3),
			},
			want: want{
				o: 3 * m,
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := (&MathTransform{Multiply: tc.multiplier}).Resolve(tc.i)

			if diff := cmp.Diff(tc.want.o, got); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestStringResolve(t *testing.T) {

	type args struct {
		fmts string
		i    interface{}
	}
	type want struct {
		o   interface{}
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"FmtString": {
			args: args{
				fmts: "verycool%s",
				i:    "thing",
			},
			want: want{
				o: "verycoolthing",
			},
		},
		"FmtInteger": {
			args: args{
				fmts: "the largest %d",
				i:    8,
			},
			want: want{
				o: "the largest 8",
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := (&StringTransform{Format: tc.fmts}).Resolve(tc.i)

			if diff := cmp.Diff(tc.want.o, got); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestConvertResolve(t *testing.T) {
	type args struct {
		ot string
		i  interface{}
	}
	type want struct {
		o   interface{}
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"StringToBool": {
			args: args{
				i:  "true",
				ot: ConvertTransformTypeBool,
			},
			want: want{
				o: true,
			},
		},
		"SameTypeNoOp": {
			args: args{
				i:  true,
				ot: ConvertTransformTypeBool,
			},
			want: want{
				o: true,
			},
		},
		"IntAliasToInt64": {
			args: args{
				i:  int64(1),
				ot: ConvertTransformTypeInt,
			},
			want: want{
				o: int64(1),
			},
		},
		"InputTypeNotSupported": {
			args: args{
				i:  []int{64},
				ot: ConvertTransformTypeString,
			},
			want: want{
				err: errors.Errorf(errFmtConvertInputTypeNotSupported, reflect.TypeOf([]int{}).Kind().String()),
			},
		},
		"ConversionPairNotSupported": {
			args: args{
				i:  "[64]",
				ot: "[]int",
			},
			want: want{
				err: errors.Errorf(errFmtConversionPairNotSupported, "string", "[]int"),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := (&ConvertTransform{ToType: tc.args.ot}).Resolve(tc.i)

			if diff := cmp.Diff(tc.want.o, got); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestGetConstantValue(t *testing.T) {
	type args struct {
		ct ConstantValue
	}

	type want struct {
		o   interface{}
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"UndefinedType": {
			args: args{
				ct: ConstantValue{},
			},
			want: want{
				o:   nil,
				err: errors.New(errConstantValueTypeNotDefined),
			},
		},
		"UnsupportedType": {
			args: args{
				ct: ConstantValue{
					Type:   "badType",
					String: pointer.StringPtr("value"),
				},
			},
			want: want{
				o:   nil,
				err: errors.Errorf(errConstantValueTypeNotSupported, "badType"),
			},
		},
		"StringType": {
			args: args{
				ct: ConstantValue{
					Type:   "string",
					String: pointer.StringPtr("crossplane"),
				},
			},
			want: want{
				o:   interface{}(pointer.StringPtr("crossplane")),
				err: nil,
			},
		},
		"StringTypeMissingValue": {
			args: args{
				ct: ConstantValue{
					Type: "string",
				},
			},
			want: want{
				o:   nil,
				err: errors.Errorf(errRequiredValue, ConstantTypeString),
			},
		},
		"IntType": {
			args: args{
				ct: ConstantValue{
					Type: "int",
					Int:  pointer.Int64Ptr(5),
				},
			},
			want: want{
				o:   interface{}(pointer.Int64Ptr(5)),
				err: nil,
			},
		},
		"IntTypeMissingValue": {
			args: args{
				ct: ConstantValue{
					Type: "int",
				},
			},
			want: want{
				o:   nil,
				err: errors.Errorf(errRequiredValue, ConstantTypeInt),
			},
		},
		"BoolType": {
			args: args{
				ct: ConstantValue{
					Type: "bool",
					Bool: pointer.BoolPtr(true),
				},
			},
			want: want{
				o:   interface{}(pointer.BoolPtr(true)),
				err: nil,
			},
		},
		"BoolTypeMissingValue": {
			args: args{
				ct: ConstantValue{
					Type: "bool",
				},
			},
			want: want{
				o:   nil,
				err: errors.Errorf(errRequiredValue, ConstantTypeBool),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.args.ct.GetValue()

			if diff := cmp.Diff(tc.want.o, got); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("Resolve(b): -want, +got:\n%s", diff)
			}
		})
	}

}

func TestPatchApply(t *testing.T) {
	now := metav1.NewTime(time.Unix(0, 0))
	lpt := fake.ConnectionDetailsLastPublishedTimer{
		Time: &now,
	}

	errNotFound := func(path string) error {
		p := &fieldpath.Paved{}
		_, err := p.GetValue(path)
		return err
	}

	type args struct {
		patch Patch
		cp    *fake.Composite
		cd    *fake.Composed
		only  []PatchType
	}
	type want struct {
		cp  *fake.Composite
		cd  *fake.Composed
		err error
	}

	cases := map[string]struct {
		reason string
		args
		want
	}{
		"InvalidCompositeFieldPathPatch": {
			reason: "Should return error when required fields not passed to applyFromFieldPathPatch",
			args: args{
				patch: Patch{
					Type: PatchTypeFromCompositeFieldPath,
				},
				cp: &fake.Composite{
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
			},
			want: want{
				err: errors.Errorf(errRequiredField, "FromFieldPath", PatchTypeFromCompositeFieldPath),
			},
		},
		"InvalidPatchType": {
			reason: "Should return an error if an invalid patch type is specified",
			args: args{
				patch: Patch{
					Type: "invalid-patchtype",
				},
				cp: &fake.Composite{
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
			},
			want: want{
				err: errors.Errorf(errInvalidPatchType, "invalid-patchtype"),
			},
		},
		"ValidCompositeFieldPathPatch": {
			reason: "Should correctly apply a CompositeFieldPathPatch with valid settings",
			args: args{
				patch: Patch{
					Type:          PatchTypeFromCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("objectMeta.labels"),
					ToFieldPath:   pointer.StringPtr("objectMeta.labels"),
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{Name: "cd"},
				},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
				},
				err: nil,
			},
		},
		"MissingOptionalFieldPath": {
			reason: "A FromFieldPath patch should be a no-op when an optional fromFieldPath doesn't exist",
			args: args{
				patch: Patch{
					Type:          PatchTypeFromCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("objectMeta.labels"),
					ToFieldPath:   pointer.StringPtr("objectMeta.labels"),
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{Name: "cd"},
				},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
					},
				},
				err: nil,
			},
		},
		"MissingRequiredFieldPath": {
			reason: "A FromFieldPath patch should return an error when a required fromFieldPath doesn't exist",
			args: args{
				patch: Patch{
					Type:          PatchTypeFromCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("wat"),
					Policy: &PatchPolicy{
						FromFieldPath: func() *FromFieldPathPolicy {
							s := FromFieldPathPolicyRequired
							return &s
						}(),
					},
					ToFieldPath: pointer.StringPtr("wat"),
				},
				cp: &fake.Composite{
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{Name: "cd"},
				},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
					},
				},
				err: errNotFound("wat"),
			},
		},
		"FilterExcludeCompositeFieldPathPatch": {
			reason: "Should not apply the patch as the PatchType is not present in filter.",
			args: args{
				patch: Patch{
					Type:          PatchTypeFromCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("objectMeta.labels"),
					ToFieldPath:   pointer.StringPtr("objectMeta.labels"),
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{Name: "cd"},
				},
				only: []PatchType{PatchTypePatchSet},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
					},
				},
				err: nil,
			},
		},
		"FilterIncludeCompositeFieldPathPatch": {
			reason: "Should apply the patch as the PatchType is present in filter.",
			args: args{
				patch: Patch{
					Type:          PatchTypeFromCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("objectMeta.labels"),
					ToFieldPath:   pointer.StringPtr("objectMeta.labels"),
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{Name: "cd"},
				},
				only: []PatchType{PatchTypeFromCompositeFieldPath},
			},
			want: want{
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
						Labels: map[string]string{
							"Test": "blah",
						}},
				},
				err: nil,
			},
		},
		"DefaultToFieldCompositeFieldPathPatch": {
			reason: "Should correctly default the ToFieldPath value if not specified.",
			args: args{
				patch: Patch{
					Type:          PatchTypeFromCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("objectMeta.labels"),
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
					},
				},
			},
			want: want{
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
						Labels: map[string]string{
							"Test": "blah",
						}},
				},
				err: nil,
			},
		},
		"ValidToCompositeFieldPathPatch": {
			reason: "Should correctly apply a ToCompositeFieldPath patch with valid settings",
			args: args{
				patch: Patch{
					Type:          PatchTypeToCompositeFieldPath,
					FromFieldPath: pointer.StringPtr("objectMeta.labels"),
					ToFieldPath:   pointer.StringPtr("objectMeta.labels"),
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
				},
			},
			want: want{
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
						Labels: map[string]string{
							"Test": "blah",
						}},
				},
				err: nil,
			},
		},
		"InvalidConstantValuePatchMissingToField": {
			reason: "Should return error when ToFieldPath not set in a FromConstantValue",
			args: args{
				patch: Patch{
					Type: PatchTypeFromConstantValue,
				},
				cp: &fake.Composite{
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
			},
			want: want{
				err: errors.Errorf(errRequiredField, "ToFieldPath", PatchTypeFromConstantValue),
			},
		},
		"InvalidConstantValuePatchMissingConstantValue": {
			reason: "Should return error when ConstantValue is not set",
			args: args{
				patch: Patch{
					Type:        PatchTypeFromConstantValue,
					ToFieldPath: pointer.StringPtr("objectMeta.generateName"),
				},
				cp: &fake.Composite{
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
			},
			want: want{
				err: errors.Errorf(errConstantValue, PatchTypeFromConstantValue),
			},
		},
		"ValidConstantValuePatchString": {
			reason: "Should patch with a constant Value",
			args: args{
				patch: Patch{
					Type:        PatchTypeFromConstantValue,
					ToFieldPath: pointer.StringPtr("objectMeta.generateName"),
					ConstantValue: &ConstantValue{
						Type:   ConstantTypeString,
						String: pointer.StringPtr("prefix"),
					},
				},
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						}},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cd",
						Labels: map[string]string{
							"Test": "blah",
						}},
				},
			},
			want: want{
				cp: &fake.Composite{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cp",
						Labels: map[string]string{
							"Test": "blah",
						},
					},
					ConnectionDetailsLastPublishedTimer: lpt,
				},
				cd: &fake.Composed{
					ObjectMeta: metav1.ObjectMeta{
						Name:         "cd",
						GenerateName: "prefix",
						Labels: map[string]string{
							"Test": "blah",
						}},
				},
				err: nil,
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ncp := tc.args.cp.DeepCopyObject()
			err := tc.args.patch.Apply(ncp, tc.args.cd, tc.args.only...)

			if tc.want.cp != nil {
				if diff := cmp.Diff(tc.want.cp, ncp); diff != "" {
					t.Errorf("\n%s\nApply(cp): -want, +got:\n%s", tc.reason, diff)
				}
			}
			if tc.want.cd != nil {
				if diff := cmp.Diff(tc.want.cd, tc.args.cd); diff != "" {
					t.Errorf("\n%s\nApply(cd): -want, +got:\n%s", tc.reason, diff)
				}
			}
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nApply(err): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestOptionalFieldPathNotFound(t *testing.T) {
	errBoom := errors.New("boom")
	errNotFound := func() error {
		p := &fieldpath.Paved{}
		_, err := p.GetValue("boom")
		return err
	}
	required := FromFieldPathPolicyRequired
	optional := FromFieldPathPolicyOptional
	type args struct {
		err error
		p   *PatchPolicy
	}

	cases := map[string]struct {
		reason string
		args
		want bool
	}{
		"NotAnError": {
			reason: "Should perform patch if no error finding field.",
			args:   args{},
			want:   false,
		},
		"NotFieldNotFoundError": {
			reason: "Should return error if something other than field not found.",
			args: args{
				err: errBoom,
			},
			want: false,
		},
		"DefaultOptionalNoPolicy": {
			reason: "Should return no-op if field not found and no patch policy specified.",
			args: args{
				err: errNotFound(),
			},
			want: true,
		},
		"DefaultOptionalNoPathPolicy": {
			reason: "Should return no-op if field not found and empty patch policy specified.",
			args: args{
				p:   &PatchPolicy{},
				err: errNotFound(),
			},
			want: true,
		},
		"OptionalNotFound": {
			reason: "Should return no-op if field not found and optional patch policy explicitly specified.",
			args: args{
				p: &PatchPolicy{
					FromFieldPath: &optional,
				},
				err: errNotFound(),
			},
			want: true,
		},
		"RequiredNotFound": {
			reason: "Should return error if field not found and required patch policy explicitly specified.",
			args: args{
				p: &PatchPolicy{
					FromFieldPath: &required,
				},
				err: errNotFound(),
			},
			want: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := IsOptionalFieldPathNotFound(tc.args.err, tc.args.p)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("IsOptionalFieldPathNotFound(...): -want, +got:\n%s", diff)
			}
		})
	}
}
