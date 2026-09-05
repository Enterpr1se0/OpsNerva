import { FormEvent, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, Edit3, Cable, LoaderCircle, Plus, Trash2 } from 'lucide-react'
import { api } from '../../../api/api'
import { localeFor } from '../../../lib/i18n'
import { PasswordInput } from '../../../components/PasswordInput'
import { useNotifier } from '../../../lib/notifications'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { errorText } from '../../../lib/utils'
import type { Proxy, ProxyInput } from '../../../types'
import { Empty, FloatingPageActions } from '../../../components/PageLayout'
import { ConfigurationEditorPage, AddressVisibilityButton } from './ConfigurationLayout'

const emptyProxyForm:ProxyInput={name:'',url:'',username:'',password:''}

export function ProxiesPage({proxies,showAddresses,onToggleAddresses,refresh}:{proxies:Proxy[];showAddresses:boolean;onToggleAddresses:()=>void;refresh:()=>Promise<void>}){
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
	const [form,setForm]=useState<ProxyInput>(emptyProxyForm)
	const [showForm,setShowForm]=useState(false)
	const [busy,setBusy]=useState('')
	const [formErrors,setFormErrors]=useState<Partial<Record<'name'|'url',string>>>({})
	const [deleteCandidate,setDeleteCandidate]=useState<Proxy|null>(null)
	const editing=!!form.id
	const editingProxy=proxies.find(proxy=>proxy.id===form.id)
	const preservesPassword=!!editingProxy?.has_password&&form.username===(editingProxy.username||'')&&!form.clear_password
	const updateProxyForm=<K extends keyof ProxyInput>(field:K,value:ProxyInput[K])=>{setForm(current=>({...current,[field]:value}));setFormErrors(current=>{if(!current[field as keyof typeof current])return current;const next={...current};delete next[field as keyof typeof next];return next})}
	const openCreate=()=>{setForm(emptyProxyForm);setFormErrors({});setShowForm(true)}
	const openEdit=(proxy:Proxy)=>{setForm({id:proxy.id,name:proxy.name,url:proxy.url,username:proxy.username||'',password:''});setFormErrors({});setShowForm(true)}
	const validateProxy=()=>{const errors:typeof formErrors={};if(!form.name.trim())errors.name=t('common.required');if(!form.url.trim())errors.url=t('common.required');else try{const url=new URL(form.url);if(!url.hostname)errors.url=t('proxies.invalidUrl')}catch{errors.url=t('proxies.invalidUrl')}setFormErrors(errors);return !Object.keys(errors).length}
	const save=async(event:FormEvent)=>{event.preventDefault();if(!validateProxy())return;setBusy('save');try{const saved=await api.saveProxy({...form,name:form.name.trim(),url:form.url.trim()});notify(t('proxies.saved',{name:saved.name}));setForm(emptyProxyForm);setFormErrors({});setShowForm(false);await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const test=async(proxy:Proxy)=>{setBusy(`test-${proxy.id}`);try{const result=await api.testProxy(proxy.id);notify(t('proxies.testPassed',{name:proxy.name,status:result.status_code||0,latency:result.latency_ms}))}catch(err){notify(errorText(err),'error')}finally{setBusy('')}}
	const remove=async()=>{const proxy=deleteCandidate;if(!proxy)return;setBusy(`delete-${proxy.id}`);try{await api.deleteProxy(proxy.id);notify(t('proxies.deleted',{name:proxy.name}));if(form.id===proxy.id){setForm(emptyProxyForm);setShowForm(false)}await refresh()}catch(err){notify(errorText(err),'error')}finally{setBusy('');setDeleteCandidate(null)}}
	return <div className="page-stack has-floating-actions">
		{!showForm&&<FloatingPageActions><AddressVisibilityButton visible={showAddresses} onToggle={onToggleAddresses}/><button className="primary" onClick={openCreate}><Plus size={16}/>{t('proxies.add')}</button></FloatingPageActions>}
		{showForm&&<ConfigurationEditorPage icon={<Cable size={22}/>} title={editing?t('proxies.editTitle'):t('proxies.createTitle')} busy={busy==='save'} onBack={()=>setShowForm(false)}><form className="proxy-form configuration-editor-form panel" noValidate onSubmit={save}><div className="form-grid proxy-fields"><label className={formErrors.name?'invalid':''}><span>{t('proxies.name')}</span><input value={form.name} maxLength={128} aria-invalid={!!formErrors.name} onChange={event=>updateProxyForm('name',event.target.value)}/>{formErrors.name&&<small className="form-field-error">{formErrors.name}</small>}</label><label className={`proxy-address-field ${formErrors.url?'invalid':''}`}><span>{t('proxies.url')}</span><input value={form.url} aria-invalid={!!formErrors.url} onChange={event=>updateProxyForm('url',event.target.value)} placeholder="socks5://127.0.0.1:1080"/>{formErrors.url&&<small className="form-field-error">{formErrors.url}</small>}</label><label><span>{t('proxies.username')}</span><input autoComplete="off" value={form.username} onChange={event=>setForm({...form,username:event.target.value,password:event.target.value?form.password:'',clear_password:false})}/></label><label><span>{t('proxies.password')}</span><PasswordInput autoComplete="new-password" value={form.password} disabled={!form.username} onChange={event=>setForm({...form,password:event.target.value,clear_password:false})} placeholder={preservesPassword?t('proxies.keepPassword'):''}/>{preservesPassword&&<small><button type="button" onClick={()=>setForm({...form,password:'',clear_password:true})}>{t('proxies.clearPassword')}</button></small>}</label></div><div className="form-actions"><button type="button" onClick={()=>setShowForm(false)}>{t('common.cancel')}</button><button className="primary" disabled={busy==='save'}>{busy==='save'?t('common.saving'):t('common.save')}</button></div></form></ConfigurationEditorPage>}
		{!showForm&&<div className="proxy-grid">{proxies.map(proxy=><article className="proxy-card panel" key={proxy.id}><div className="proxy-card-head"><div><Cable size={20}/></div><span><h3>{proxy.name}</h3><code>{showAddresses?proxy.url:'••••••'}</code></span>{proxy.ssh_compatible&&<em>SSH</em>}</div><dl><div><dt>{t('proxies.authentication')}</dt><dd>{proxy.username?`${proxy.username}${proxy.has_password?` · ${t('proxies.passwordSaved')}`:''}`:t('proxies.noAuthentication')}</dd></div><div><dt>{t('common.updated')}</dt><dd>{new Date(proxy.updated_at).toLocaleString(localeFor(instance.language))}</dd></div></dl><div className="card-actions"><button disabled={!!busy} onClick={()=>void test(proxy)}>{busy===`test-${proxy.id}`?<LoaderCircle className="spin" size={14}/>:<Activity size={14}/>} {t('common.test')}</button><button disabled={!!busy} onClick={()=>openEdit(proxy)}><Edit3 size={14}/>{t('common.edit')}</button><button className="danger" disabled={!!busy} title={t('common.delete')} onClick={()=>setDeleteCandidate(proxy)}><Trash2 size={14}/></button></div></article>)}</div>}
		{!showForm&&!proxies.length&&<Empty icon={<Cable/>} title={t('proxies.emptyTitle')}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('proxies.deleteTitle',{name:deleteCandidate.name})} busy={busy===`delete-${deleteCandidate.id}`} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
	</div>
}
