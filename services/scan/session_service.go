package scan

import (
	"fmt"
	"net/http"

	"github.com/Sirupsen/logrus"
	"github.com/emicklei/go-restful"
	"github.com/facebookgo/stackerr"

	"github.com/bearded-web/bearded/models/report"
	"github.com/bearded-web/bearded/models/scan"
	"github.com/bearded-web/bearded/pkg/manager"
	"github.com/bearded-web/bearded/services"
)

func (s *ScanService) RegisterSessions(ws *restful.WebService) {
	r := ws.GET(fmt.Sprintf("{%s}/sessions/{%s}", ParamId, SessionParamId)).To(s.TakeScan(s.TakeSession(s.sessionGet)))
	r.Doc("sessionGet")
	r.Operation("sessionGet")
	addDefaults(r)
	r.Param(ws.PathParameter(ParamId, ""))
	r.Param(ws.PathParameter(SessionParamId, ""))
	r.Writes(scan.Session{})
	r.Do(services.Returns(
		http.StatusOK,
		http.StatusNotFound))
	r.Do(services.ReturnsE(http.StatusBadRequest))
	ws.Route(r)

	r = ws.PUT(fmt.Sprintf("{%s}/sessions/{%s}", ParamId, SessionParamId)).To(s.TakeScan(s.TakeSession(s.sessionUpdate)))
	r.Doc("sessionUpdate")
	r.Operation("sessionUpdate")
	addDefaults(r)
	r.Param(ws.PathParameter(ParamId, ""))
	r.Param(ws.PathParameter(SessionParamId, ""))
	r.Reads(SessionUpdateEntity{})
	r.Writes(scan.Session{})
	r.Do(services.Returns(
		http.StatusOK,
		http.StatusNotFound))
	r.Do(services.ReturnsE(http.StatusBadRequest))
	ws.Route(r)

	r = ws.GET(fmt.Sprintf("{%s}/sessions/{%s}/report", ParamId, SessionParamId)).To(s.TakeScan(s.TakeSession(s.sessionReportGet)))
	r.Doc("sessionReportGet")
	r.Operation("sessionReportGet")
	addDefaults(r)
	r.Param(ws.PathParameter(ParamId, ""))
	r.Param(ws.PathParameter(SessionParamId, ""))
	r.Writes(report.Report{})
	r.Do(services.Returns(
		http.StatusOK,
		http.StatusNotFound))
	r.Do(services.ReturnsE(http.StatusBadRequest))
	ws.Route(r)

	r = ws.POST(fmt.Sprintf("{%s}/sessions/{%s}/report", ParamId, SessionParamId)).To(s.TakeScan(s.TakeSession(s.sessionReportCreate)))
	r.Doc("sessionReportCreate")
	r.Operation("sessionReportCreate")
	addDefaults(r)
	r.Param(ws.PathParameter(ParamId, ""))
	r.Param(ws.PathParameter(SessionParamId, ""))
	r.Reads(report.Report{})
	r.Writes(report.Report{})
	r.Do(services.Returns(http.StatusCreated))
	r.Do(services.ReturnsE(
		http.StatusBadRequest,
		http.StatusConflict))
	ws.Route(r)
}

func (s *ScanService) sessionGet(_ *restful.Request, resp *restful.Response, _ *scan.Scan, sess *scan.Session) {
	resp.WriteHeader(http.StatusOK)
	resp.WriteEntity(sess)
}

func (s *ScanService) sessionUpdate(req *restful.Request, resp *restful.Response, sc *scan.Scan, sess *scan.Session) {
	// TODO (m0sth8): Check permissions
	// TODO (m0sth8): Forbid changes in session after finished|failed status

	raw := &SessionUpdateEntity{}

	if err := req.ReadEntity(raw); err != nil {
		logrus.Warn(stackerr.Wrap(err))
		resp.WriteServiceError(http.StatusBadRequest, services.WrongEntityErr)
		return
	}
	if !(raw.Status == scan.StatusWorking || raw.Status == scan.StatusFinished || raw.Status == scan.StatusFailed) {
		resp.WriteServiceError(http.StatusBadRequest, services.NewBadReq("status should be one of [working|finished|failed]"))
		return
	}

	mgr := s.Manager()
	defer mgr.Close()

	logrus.Debugf("Update session %s status from %s to %s", mgr.FromId(sess.Id), sess.Status, raw.Status)

	sess.Status = raw.Status
	if err := mgr.Scans.UpdateSession(sc, sess); err != nil {
		logrus.Error(stackerr.Wrap(err))
		resp.WriteServiceError(http.StatusInternalServerError, services.DbErr)
		return
	}
	s.Scheduler().UpdateScan(sc)

	if err := mgr.Feed.UpdateScan(sc); err != nil {
		logrus.Error(stackerr.Wrap(err))
	}

	resp.WriteEntity(sess)
}

func (s *ScanService) sessionReportGet(_ *restful.Request, resp *restful.Response, _ *scan.Scan, sess *scan.Session) {

	mgr := s.Manager()
	defer mgr.Close()

	rep, err := mgr.Reports.GetBySession(sess.Id)
	if err != nil {
		if mgr.IsNotFound(err) {
			resp.WriteErrorString(http.StatusNotFound, "Not found")
			return
		}
		logrus.Error(stackerr.Wrap(err))
		resp.WriteServiceError(http.StatusInternalServerError, services.DbErr)
		return
	}

	resp.WriteHeader(http.StatusOK)
	resp.WriteEntity(rep)
}

func (s *ScanService) sessionReportCreate(req *restful.Request, resp *restful.Response, sc *scan.Scan, sess *scan.Session) {
	// TODO (m0sth8): Check permissions
	// TODO (m0sth8): Forbid creating report in session after finished|failed status

	raw := &report.Report{}

	if err := req.ReadEntity(raw); err != nil {
		logrus.Error(stackerr.Wrap(err))
		resp.WriteServiceError(http.StatusBadRequest, services.WrongEntityErr)
		return
	}

	raw.Scan = sc.Id
	raw.ScanSession = sess.Id

	mgr := s.Manager()
	defer mgr.Close()

	rep, err := mgr.Reports.Create(raw)

	if err != nil {
		if mgr.IsDup(err) {
			resp.WriteServiceError(
				http.StatusConflict,
				services.NewError(services.CodeDuplicate, "report with this scan session is existed"))
			return
		}
		logrus.Error(stackerr.Wrap(err))
		resp.WriteServiceError(http.StatusInternalServerError, services.DbErr)
		return
	}

	resp.WriteHeader(http.StatusCreated)
	resp.WriteEntity(rep)
}

// Helpers

type SessionFunction func(*restful.Request, *restful.Response, *scan.Scan, *scan.Session)

// Decorate ScanFunction. Look for session in scan by SessionParamId
// and add session object in the end. If session is not found then return Not Found.
func (s *ScanService) TakeSession(fn SessionFunction) ScanFunction {
	return func(req *restful.Request, resp *restful.Response, sc *scan.Scan) {
		id := req.PathParameter(SessionParamId)
		if !s.IsId(id) {
			resp.WriteServiceError(http.StatusBadRequest, services.IdHexErr)
			return
		}

		objId := manager.ToId(id)

		for _, sess := range sc.Sessions {
			if sess.Id == objId {
				fn(req, resp, sc, sess)
				return
			}
		}
		resp.WriteErrorString(http.StatusNotFound, "Not found")
		return
	}
}
