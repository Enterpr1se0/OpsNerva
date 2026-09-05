import { FormEvent, Suspense, useEffect, useMemo, useState, lazy } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { BookOpen, Check, ChevronRight, FileText, LoaderCircle, RefreshCw, Save, Search, ShieldAlert, Trash2, UploadCloud, X } from 'lucide-react'
import { api } from '../../../api/api'
import { localeFor } from '../../../lib/i18n'
import { useNotifier } from '../../../lib/notifications'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { errorText, formatFileSize } from '../../../lib/utils'
import type { LLMToolCatalog, ManagedSkill } from '../../../types'
import { FloatingPageActions } from '../../../components/PageLayout'

const MarkdownMessage=lazy(()=>import('../../../components/MarkdownMessage').then(module=>({default:module.MarkdownMessage})))

export function SkillsPage({skills,refreshSkills,refreshToolCatalog,onToolCatalogChanged}:{skills:ManagedSkill[];refreshSkills:()=>Promise<void>;refreshToolCatalog:()=>Promise<void>;onToolCatalogChanged:(catalog:LLMToolCatalog)=>void}){
	const {t,i18n:instance}=useTranslation()
	const notify=useNotifier()
	const [query,setQuery]=useState('')
	const [selectedName,setSelectedName]=useState(()=>skills[0]?.name||'')
	const [selected,setSelected]=useState<ManagedSkill|null>(null)
	const [draft,setDraft]=useState('')
	const [loading,setLoading]=useState(()=>skills.length>0)
	const [saving,setSaving]=useState(false)
	const [uploading,setUploading]=useState(false)
	const [reloading,setReloading]=useState(false)
	const [uploadOpen,setUploadOpen]=useState(false)
	const [uploadName,setUploadName]=useState('')
	const [uploadFile,setUploadFile]=useState<File|null>(null)
	const [deleteName,setDeleteName]=useState('')
	const [deleting,setDeleting]=useState(false)
	const [toggling,setToggling]=useState(false)
	const [error,setError]=useState('')
	const filtered=useMemo(()=>{const needle=query.trim().toLowerCase();return skills.filter(skill=>!needle||`${skill.name} ${skill.summary}`.toLowerCase().includes(needle))},[skills,query])
	useEffect(()=>{if(!skills.length){setSelectedName('');setSelected(null);setDraft('');return}if(!selectedName||!skills.some(skill=>skill.name===selectedName))setSelectedName(skills[0].name)},[skills,selectedName])
	useEffect(()=>{if(!selectedName)return;let cancelled=false;setLoading(true);setError('');api.skill(selectedName).then(skill=>{if(cancelled)return;setSelected(skill);setDraft(skill.content||'')}).catch(err=>{if(!cancelled)setError(errorText(err))}).finally(()=>{if(!cancelled)setLoading(false)});return()=>{cancelled=true}},[selectedName])
	const dirty=!!selected&&draft!==selected.content
	const markdownUpload=!!uploadFile&&/\.(?:md|markdown)$/i.test(uploadFile.name)
	const selectFile=(file:File|null)=>{setUploadFile(file);if(file&&/\.(?:md|markdown)$/i.test(file.name)&&!uploadName){const base=file.name.replace(/\.(markdown|md)$/i,'').replace(/[^A-Za-z0-9_.-]+/g,'-').replace(/^-+|-+$/g,'').slice(0,64);setUploadName(base)}else if(file)setUploadName('')}
	const openUpload=()=>{setUploadOpen(true);setError('')}
	const closeUpload=()=>{if(uploading)return;setUploadOpen(false);setUploadName('');setUploadFile(null);setError('')}
	const upload=async(event:FormEvent)=>{event.preventDefault();if(!uploadFile)return;if(markdownUpload&&!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(uploadName.trim())){setError(t('skills.invalidName'));return}setUploading(true);setError('');try{const results=await api.uploadSkill(uploadName.trim(),uploadFile);const result=results[0]!;await Promise.all([refreshSkills(),refreshToolCatalog()]);setSelectedName(result.name);setSelected(result);setDraft(result.content||'');setUploadOpen(false);setUploadName('');setUploadFile(null);notify(t(results.length===1?'skills.uploaded':'skills.uploadedMany',{name:result.name,count:results.length}))}catch(err){setError(errorText(err))}finally{setUploading(false)}}
	const save=async()=>{if(!selected)return;setSaving(true);setError('');try{const result=await api.saveSkill(selected.name,draft);setSelected(result);setDraft(result.content||'');await refreshSkills();notify(t('skills.saved',{name:result.name}))}catch(err){setError(errorText(err))}finally{setSaving(false)}}
	const permanentlyDelete=async()=>{if(!deleteName)return;setDeleting(true);setError('');try{await api.deleteSkill(deleteName);setDeleteName('');setSelectedName('');setSelected(null);setDraft('');await refreshSkills();notify(t('skills.deleted',{name:deleteName}))}catch(err){setError(errorText(err))}finally{setDeleting(false)}}
	const toggleEnabled=async()=>{if(!selected)return;setToggling(true);setError('');try{const result=await api.setSkillEnabled(selected.name,!selected.enabled);setSelected(result);setDraft(result.content||draft);await refreshSkills();notify(t(result.enabled?'skills.toggledEnabled':'skills.toggledDisabled',{name:result.name}))}catch(err){setError(errorText(err))}finally{setToggling(false)}}
	const reload=async()=>{setReloading(true);setError('');try{onToolCatalogChanged(await api.reloadSkills());await refreshSkills();notify(t('skills.reloaded'))}catch(err){setError(errorText(err))}finally{setReloading(false)}}

	return <div className="skills-page page-stack has-floating-actions">
			<FloatingPageActions><button onClick={()=>void reload()} disabled={reloading}><RefreshCw className={reloading?'spin':''} size={15}/>{reloading?t('common.refreshing'):t('skills.reload')}</button><button className="primary" onClick={openUpload}><UploadCloud size={15}/>{t('skills.uploadSkill')}</button></FloatingPageActions>
		{error&&!uploadOpen&&<div className="skill-error"><ShieldAlert size={15}/>{error}<button onClick={()=>setError('')}><X size={14}/></button></div>}
		{uploadOpen&&createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)closeUpload()}}><form className="connection-dialog compact panel skill-upload-panel skill-upload-dialog" role="dialog" aria-modal="true" aria-labelledby="skill-upload-title" noValidate onSubmit={upload}><header><span><UploadCloud size={19}/><span><h2 id="skill-upload-title">{t('skills.uploadPackage')}</h2></span></span><button type="button" disabled={uploading} onClick={closeUpload} title={t('common.close')}><X size={15}/></button></header><div className="skill-upload-dialog-body">{markdownUpload&&<label><span>{t('skills.skillName')}</span><input value={uploadName} onChange={event=>setUploadName(event.target.value)} autoFocus/></label>}<label className="skill-file-picker"><FileText size={15}/><span><b>{uploadFile?.name||t('skills.choosePackage')}</b><small>{uploadFile?formatFileSize(uploadFile.size):t('skills.maxPackage')}</small></span><input type="file" accept=".md,.markdown,.zip,.7z,text/markdown,application/zip,application/x-7z-compressed" onChange={event=>selectFile(event.target.files?.[0]||null)}/></label></div>{error&&<div className="connection-dialog-error" role="alert"><ShieldAlert size={14}/><span>{error}</span></div>}<footer><button type="button" disabled={uploading} onClick={closeUpload}>{t('common.cancel')}</button><button className="primary" disabled={uploading||!uploadFile||(markdownUpload&&!uploadName.trim())}>{uploading?<LoaderCircle className="spin" size={14}/>:<UploadCloud size={14}/>} {uploading?t('common.uploading'):t('skills.uploadActivate')}</button></footer></form></div>,document.body)}
		<div className="skill-manager-layout">
			<section className="skill-list-panel panel"><label className="skill-search"><Search size={14}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('skills.search')}/></label><div className="skill-list">{filtered.length?filtered.map(skill=><button className={`${selectedName===skill.name?'active':''} ${skill.enabled?'':'disabled'}`} onClick={()=>setSelectedName(skill.name)} key={skill.name}><div className="skill-card-icon"><BookOpen size={16}/></div><span><code>{skill.name}</code>{skill.summary&&<p>{skill.summary}</p>}<small><em className={skill.enabled?'enabled':'disabled'}>{skill.enabled?t('common.enabled'):t('common.disabled')}</em>{skill.file_count||1} {t('common.files')} · {formatFileSize(skill.size_bytes||0)}{skill.updated_at?` · ${new Date(skill.updated_at).toLocaleDateString(localeFor(instance.language))}`:''}</small></span><ChevronRight size={14}/></button>):<div className="skill-list-empty"><BookOpen size={23}/><b>{skills.length?t('skills.noMatch'):t('skills.noneInstalled')}</b></div>}</div></section>
				<section className="skill-editor panel">{loading?<div className="skill-editor-state"><LoaderCircle className="spin" size={21}/>{t('skills.loading')}</div>:selected?<><header><div><BookOpen size={17}/><span><small>{t('skills.managed')} · {selected.enabled?t('common.enabled'):t('common.disabled')}</small><code>{selected.name}</code></span></div><section><button className={selected.enabled?'skill-disable':'skill-enable'} disabled={toggling} onClick={toggleEnabled}>{toggling?<LoaderCircle className="spin" size={13}/>:selected.enabled?<X size={13}/>:<Check size={13}/>} {selected.enabled?t('common.disable'):t('common.enable')}</button><button disabled={!dirty||saving} onClick={save}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('skills.saveChanges')}</button><button className="danger" onClick={()=>setDeleteName(selected.name)}><Trash2 size={13}/>{t('common.delete')}</button></section></header><div className="skill-editor-meta"><span><b>SHA256</b><code title={selected.content_sha256}>{selected.content_sha256?.slice(0,16)||'—'}</code></span><span><b>{t('common.files')}</b><code>{selected.file_count||1}</code></span><span><b>{t('common.size')}</b><code>{formatFileSize(selected.size_bytes||0)}</code></span><span><b>{t('common.updated')}</b><code>{selected.updated_at?new Date(selected.updated_at).toLocaleString(localeFor(instance.language)):'—'}</code></span></div><div className="skill-editor-split"><label><span>SKILL.md</span><textarea value={draft} spellCheck={false} onChange={event=>setDraft(event.target.value)}/></label><section><span>{t('skills.livePreview')}</span><div className="markdown-body"><Suspense fallback={draft||t('skills.emptySkill')}><MarkdownMessage content={draft||t('skills.emptySkill')} scope="skills"/></Suspense></div></section></div></>:<div className="skill-editor-state"><BookOpen size={25}/><b>{t('skills.select')}</b></div>}</section>
		</div>
		{deleteName&&<DestructiveConfirmDialog title={t('skills.deleteTitle',{name:deleteName})} busy={deleting} onCancel={()=>setDeleteName('')} onConfirm={()=>void permanentlyDelete()}/>}
	</div>
}
