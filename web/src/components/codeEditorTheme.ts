import { HighlightStyle, syntaxHighlighting } from '@codemirror/language'
import { EditorView } from '@codemirror/view'
import { tags } from '@lezer/highlight'

const editorViewTheme=EditorView.theme({
	'&':{height:'100%',backgroundColor:'var(--code-editor-bg)',color:'var(--code-editor-text)'},
	'&.cm-focused':{outline:'none'},
	'.cm-scroller':{overflow:'auto',fontFamily:"'DM Mono','Microsoft YaHei UI','Microsoft YaHei','PingFang SC','Noto Sans CJK SC','Noto Sans SC Variable',monospace",fontSize:'var(--text-control)',lineHeight:'1.7'},
	'.cm-content':{minHeight:'100%',padding:'18px',caretColor:'var(--code-editor-caret)'},
	'.cm-line':{padding:'0'},
	'.cm-cursor,.cm-dropCursor':{borderLeftColor:'var(--code-editor-caret)'},
	'&.cm-focused .cm-selectionBackground,.cm-selectionBackground,.cm-content ::selection':{backgroundColor:'var(--code-editor-selection) !important'},
	'.cm-activeLine':{backgroundColor:'var(--code-editor-active-line)'},
})

const editorHighlightStyle=HighlightStyle.define([
	{tag:tags.comment,color:'var(--code-editor-comment)',fontStyle:'italic'},
	{tag:[tags.keyword,tags.operatorKeyword,tags.modifier,tags.controlKeyword,tags.definitionKeyword],color:'var(--code-editor-keyword)'},
	{tag:[tags.string,tags.regexp,tags.escape],color:'var(--code-editor-string)'},
	{tag:[tags.number,tags.bool,tags.null],color:'var(--code-editor-number)'},
	{tag:[tags.typeName,tags.className,tags.namespace],color:'var(--code-editor-type)'},
	{tag:[tags.definition(tags.variableName),tags.function(tags.variableName)],color:'var(--code-editor-function)'},
	{tag:[tags.variableName,tags.propertyName,tags.attributeName],color:'var(--code-editor-variable)'},
	{tag:[tags.operator,tags.punctuation],color:'var(--code-editor-operator)'},
	{tag:[tags.heading,tags.link,tags.url],color:'var(--code-editor-link)'},
	{tag:tags.emphasis,fontStyle:'italic'},
	{tag:tags.strong,fontWeight:'700'},
	{tag:tags.meta,color:'var(--code-editor-meta)'},
	{tag:tags.invalid,color:'var(--code-editor-invalid)'},
])

export const codeEditorTheme=[editorViewTheme,syntaxHighlighting(editorHighlightStyle)]
