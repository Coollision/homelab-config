package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *AWSTunnelStack) DeepCopyInto(out *AWSTunnelStack) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *AWSTunnelStack) DeepCopy() *AWSTunnelStack {
	if in == nil {
		return nil
	}
	out := new(AWSTunnelStack)
	in.DeepCopyInto(out)
	return out
}

func (in *AWSTunnelStack) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AWSTunnelStackList) DeepCopyInto(out *AWSTunnelStackList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]AWSTunnelStack, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *AWSTunnelStackList) DeepCopy() *AWSTunnelStackList {
	if in == nil {
		return nil
	}
	out := new(AWSTunnelStackList)
	in.DeepCopyInto(out)
	return out
}

func (in *AWSTunnelStackList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AWSTunnelStackSpec) DeepCopyInto(out *AWSTunnelStackSpec) {
	*out = *in
	if in.AWS.ExtraProfile != nil {
		in, out := &in.AWS.ExtraProfile, &out.AWS.ExtraProfile
		*out = make([]AWSProfileSpec, len(*in))
		copy(*out, *in)
	}
	in.Auth.Resources.DeepCopyInto(&out.Auth.Resources)
	in.Auth.InitResources.DeepCopyInto(&out.Auth.InitResources)
	in.TunnelDefaults.Resources.DeepCopyInto(&out.TunnelDefaults.Resources)
	in.TunnelDefaults.ProxyResources.DeepCopyInto(&out.TunnelDefaults.ProxyResources)
	if in.Tunnels != nil {
		in, out := &in.Tunnels, &out.Tunnels
		*out = make([]TunnelSpec, len(*in))
		for i := range *in {
			(*out)[i] = (*in)[i]
			(*in)[i].Resources.DeepCopyInto(&(*out)[i].Resources)
			(*in)[i].ProxyResources.DeepCopyInto(&(*out)[i].ProxyResources)
		}
	}
}
